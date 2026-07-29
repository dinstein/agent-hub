package confops

import (
	"context"
	"fmt"
	"net/netip"
	neturl "net/url"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/guard/netguard"
	"github.com/dinstein/agent-hub/internal/guard/spawnguard"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// ServerSpec is one downstream server as an operation takes it: the id plus
// the registry entry. The entry is stored VERBATIM — ${SECRET_X} placeholders
// in Env/Headers are resolved at connect time in internal/downstream, never
// here, because a registry document must never hold a credential.
type ServerSpec struct {
	ID    string
	Entry registry.ServerEntry
}

// ServerResult is what the server operations return: the common Result plus
// the entries as they now stand on disk.
type ServerResult struct {
	Result
	Servers []ServerSpec
}

// ValidateServerSpec is the whole configuration-time check chain for one
// entry, and the ONLY place the two entry shapes are defined:
//
//	stdio    Command required; the HTTP-only fields must be absent.
//	http/sse URL required and screened; the stdio-only fields must be absent.
//
// then the runtime half (registry.ValidateRuntime), the generated
// `docker run` line, and the spawn guard's screen of that line.
//
// It runs BEFORE anything is opened or written, so a rejected entry never
// leaves a half-written registry behind and the operator can still fix what
// they typed.
//
// Failure direction: every unknown or contradictory combination is REFUSED.
// Silently ignoring a flag is how an operator loses the isolation they asked
// for without ever seeing a message.
func ValidateServerSpec(spec ServerSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return usagef("a server id is required")
	}
	e := spec.Entry
	switch e.TransportName() {
	case registry.TransportStdio:
		if strings.TrimSpace(e.Command) == "" {
			return usagef("server %q: the stdio transport needs a command", spec.ID)
		}
		if e.URL != "" || len(e.Headers) > 0 {
			return usagef("server %q: url and headers apply to the http and sse transports only", spec.ID)
		}
	case registry.TransportHTTP, registry.TransportSSE:
		if e.Docker != nil || e.Runtime != "" {
			return usagef("server %q: the container runtime applies to the stdio transport only", spec.ID)
		}
		if strings.TrimSpace(e.URL) == "" {
			return usagef("server %q: the %s transport needs a url", spec.ID, e.TransportName())
		}
		if e.Command != "" || len(e.Args) > 0 || len(e.Env) > 0 || e.Cwd != "" {
			return usagef("server %q: command/args/env/cwd apply to the stdio transport only", spec.ID)
		}
		if err := ValidateEndpoint(e.URL, e.Provenance == registry.ProvenanceLocal); err != nil {
			return err
		}
	default:
		return &Error{
			Kind: KindUsage, Code: CodeUnsupportedTransport,
			Message: fmt.Sprintf("unknown transport %q", e.Transport),
			Hint:    "supported transports: stdio, http (streamable-http), sse (legacy HTTP+SSE)",
		}
	}
	if err := ValidateOAuthHint(spec.ID, e.OAuth, e.Provenance == registry.ProvenanceLocal); err != nil {
		return err
	}
	return ValidateDockerEntry(spec.ID, e)
}

// ValidateOAuthHint screens the login hints of one entry. It runs for EVERY
// transport on purpose: an stdio child that proxies to a remote authorization
// server carries the same hints as an http endpoint does, so the hints are
// transport-independent (registry.OAuthHint) and so is their check.
//
// The three fields are pins that shortcut discovery, which is exactly why a
// malformed one is refused rather than carried: a typo in an issuer surfaces
// much later as an unrelated-looking `auth login` failure against a URL the
// operator never sees. local mirrors the entry's provenance — a local
// server's authorization server may legitimately be loopback too, and
// nothing else is unblocked.
//
// A nil hint is the norm and always valid: discovery works from the server
// URL alone (RFC 9728).
func ValidateOAuthHint(id string, h *registry.OAuthHint, local bool) error {
	if h == nil {
		return nil
	}
	if h.Issuer != "" {
		// RFC 8414 §2: an issuer identifier carries no query and no
		// fragment. Metadata URLs are derived from it by path insertion, so
		// a query string here silently produces a 404 at discovery time.
		if err := validateHintURL(id, "issuer", h.Issuer, local, true); err != nil {
			return err
		}
	}
	if h.ResourceMetadataURL != "" {
		if err := validateHintURL(id, "resourceMetadataUrl", h.ResourceMetadataURL, local, false); err != nil {
			return err
		}
	}
	for _, s := range h.Scopes {
		// RFC 6749 §3.3: scopes are space-delimited, so a value holding
		// whitespace is not one scope but a set that would be sent as a
		// single unknown token and rejected by the authorization server.
		if strings.TrimSpace(s) == "" {
			return usagef("server %q: an oauth scope must not be empty", id)
		}
		if strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == '"' || r == '\\' }) {
			return usagef("server %q: oauth scope %q contains whitespace or a quote; "+
				"pass one scope per value", id, s)
		}
	}
	return nil
}

// validateHintURL screens one OAuth URL pin with the same predicate
// `server add --url` uses, so a hint cannot point at an address the
// connector would refuse anyway.
func validateHintURL(id, field, raw string, local, noQueryOrFragment bool) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return usagef("server %q: oauth %s %q is not a URL: %v", id, field, raw, err)
	}
	// https unless the entry is a local server: an OAuth pin is where the
	// authorization code and the token itself travel, so plaintext is
	// refused everywhere a network can see it. The MCP endpoint check
	// (ValidateEndpoint) is deliberately laxer — it does not carry the
	// credential exchange.
	if u.Scheme != "https" && (!local || u.Scheme != "http") {
		return usagef("server %q: oauth %s scheme %q must be https (http only for a --local server)",
			id, field, u.Scheme)
	}
	if u.Host == "" {
		return usagef("server %q: oauth %s %q has no host", id, field, raw)
	}
	if noQueryOrFragment && (u.RawQuery != "" || u.Fragment != "") {
		return usagef("server %q: oauth %s %q must not carry a query or fragment (RFC 8414 §2)",
			id, field, raw)
	}
	if !local && netguard.HostIsDefinitelyPrivate(u.Host) {
		e := usagef("server %q: oauth %s %s is a private address", id, field, u.Host)
		if isLoopbackHost(u.Hostname()) {
			e.Hint = "pass --local if this really is a server running on this machine"
		}
		return e
	}
	return nil
}

// ValidateEndpoint rejects an endpoint the connector would refuse anyway, at
// the moment the operator can still fix it.
//
// It screens with HostIsDefinitelyPrivate (literals and the localhost tree,
// NO DNS) rather than the fail-closed HostIsPrivate the connector uses. The
// two questions differ: adding a server is a configuration edit that must
// work on a laptop with no network and on a name that only resolves inside a
// VPN, so refusing everything unresolvable here would break honest
// workflows. The security boundary stays where it belongs — internal/
// downstream screens the name before connecting AND the resolved address at
// dial time, which is the only check DNS rebinding cannot walk around.
//
// local is deliberately narrow: it unblocks a LITERAL loopback URL only,
// never RFC1918 and never a name whose DNS answer claims to be local,
// because those are the ranges cloud metadata services and intranet hosts
// live in.
func ValidateEndpoint(raw string, local bool) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return usagef("--url %q is not a URL: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return usagef("--url scheme %q must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return usagef("--url %q has no host", raw)
	}
	loopback := isLoopbackHost(u.Hostname())
	if local && !loopback {
		return usagef("--local only covers literal loopback URLs, not %q", u.Host)
	}
	if !local && netguard.HostIsDefinitelyPrivate(u.Host) {
		e := usagef("--url %s is a private address", u.Host)
		if loopback {
			e.Hint = "pass --local if this really is a server running on this machine"
		}
		return e
	}
	return nil
}

// isLoopbackHost reports whether a URL host is certainly loopback: an IP
// literal, or the RFC 6761 localhost tree (reserved for loopback by spec).
// Names are never resolved — a DNS answer can deny trust, never confer it.
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().IsLoopback()
	}
	l := strings.ToLower(strings.TrimSuffix(h, "."))
	return l == "localhost" || strings.HasSuffix(l, ".localhost")
}

// DockerConfigFor maps a registry entry onto the spawner's container
// configuration. The container's environment IS the entry's Env (one place
// to look, one place to put a ${SECRET_X} placeholder); the spawner forwards
// those variables by name so their values never reach argv.
func DockerConfigFor(id string, e registry.ServerEntry) transport.DockerConfig {
	cfg := transport.DockerConfig{ServerID: id, Env: e.Env}
	if e.Docker == nil {
		return cfg
	}
	cfg.Image = e.Docker.Image
	cfg.Network = e.Docker.Network
	cfg.Memory = e.Docker.Memory
	cfg.CPUs = e.Docker.CPUs
	cfg.User = e.Docker.User
	// The entry's cwd is a path inside the container, and SpawnDocker reads
	// it as --workdir. Rendering it the same way here is what keeps the run
	// line the operator is shown identical to the one that will be spawned.
	cfg.Workdir = e.Docker.Workdir
	if cfg.Workdir == "" {
		cfg.Workdir = e.Cwd
	}
	cfg.ExtraRunArgs = e.Docker.ExtraArgs
	for _, m := range e.Docker.Mounts {
		cfg.Mounts = append(cfg.Mounts, transport.Mount{Source: m.Source, Target: m.Target, Write: m.Write})
	}
	return cfg
}

// DockerRunLine renders the `docker run` argv a docker-runtime entry would
// produce. Validation, `server inspect` and the spawn-guard screen all read
// the same line, so what the operator is shown is what the guard judged.
func DockerRunLine(id string, e registry.ServerEntry) ([]string, error) {
	return transport.BuildDockerRunArgs(DockerConfigFor(id, e), e.Command, e.Args)
}

// ValidateDockerEntry runs the runtime half of the check chain: runtime
// shape → container config → spawn-guard screen.
//
// The guard is invoked with the literal command name "docker" rather than a
// resolved path: this is a check on the SHAPE of the run line, and it must
// work identically on a machine that has no docker installed (adding a
// server and running it are separate acts, often on separate machines).
func ValidateDockerEntry(id string, e registry.ServerEntry) error {
	if err := e.ValidateRuntime(); err != nil {
		return dockerConfigError(id, err)
	}
	if !e.IsDocker() {
		return nil
	}
	args, err := DockerRunLine(id, e)
	if err != nil {
		return dockerConfigError(id, err)
	}
	env := make([]string, 0, len(e.Env))
	for k, v := range e.Env {
		env = append(env, k+"="+v)
	}
	if err := spawnguard.New(spawnguard.Config{}).Check("docker", args, env); err != nil {
		return &Error{
			Kind: KindDenied, Code: CodeDenied,
			Message: fmt.Sprintf("server %q: %v", id, err),
			Hint:    "the spawn guard screens the generated 'docker run' line; remove the offending mount or flag",
		}
	}
	return nil
}

func dockerConfigError(id string, err error) *Error {
	return &Error{
		Kind: KindUsage, Code: CodeUsage,
		Message: fmt.Sprintf("server %q: %v", id, err),
		Hint:    "see 'agenthub server add --help' for the docker runtime flags",
	}
}

// AddServer registers one new server. It refuses to overwrite: an existing
// id is a conflict, never a silent replacement (use UpdateServer for that).
func AddServer(ctx context.Context, st *registry.Store, spec ServerSpec, pre Precondition) (ServerResult, error) {
	return AddServers(ctx, st, []ServerSpec{spec}, pre)
}

// AddServers registers several servers in ONE transaction, which is what
// pasting an `mcpServers` fragment needs: either every entry lands or none
// does, so a conflict halfway through cannot leave a partial import behind.
func AddServers(ctx context.Context, st *registry.Store, specs []ServerSpec, pre Precondition) (ServerResult, error) {
	if len(specs) == 0 {
		return ServerResult{}, usagef("no server entries to add")
	}
	for _, spec := range specs {
		if err := ValidateServerSpec(spec); err != nil {
			return ServerResult{}, err
		}
	}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
		}
		for _, spec := range specs {
			if _, exists := tx.Servers.V.Servers[spec.ID]; exists {
				e := conflictf(CodeServerExists, "server %q already exists", spec.ID)
				e.Hint = fmt.Sprintf("remove it first with 'agenthub server rm %s'", spec.ID)
				return e
			}
			tx.Servers.V.Servers[spec.ID] = registry.Doc[registry.ServerEntry]{V: spec.Entry}
		}
		return nil
	})
	if err != nil {
		return ServerResult{Result: res}, err
	}
	return ServerResult{Result: res, Servers: append([]ServerSpec(nil), specs...)}, nil
}

// UpdateServer replaces an existing server's definition wholesale.
//
// Wholesale, not field-by-field, on purpose: an entry's fields are not
// independent (a transport decides which half of the struct is meaningful),
// so a per-field patch could build a shape neither transport accepts. The
// caller reads the current entry, edits it, and hands back a complete one —
// with the Precondition making that read-modify-write safe against a
// concurrent writer.
func UpdateServer(ctx context.Context, st *registry.Store, spec ServerSpec, pre Precondition) (ServerResult, error) {
	if err := ValidateServerSpec(spec); err != nil {
		return ServerResult{}, err
	}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Servers.V.Servers[spec.ID]
		if !ok {
			return serverNotFound(spec.ID)
		}
		doc.V = spec.Entry
		tx.Servers.V.Servers[spec.ID] = doc
		return nil
	})
	if err != nil {
		return ServerResult{Result: res}, err
	}
	return ServerResult{Result: res, Servers: []ServerSpec{spec}}, nil
}

// RemoveServer deletes a server and everything the hub knows about it: the
// registry entry, every reference to it in the surviving registry documents,
// its stored credentials, and its out-of-registry state (fingerprint pins,
// approval records, tool cache).
//
// "Everything" is the point. An earlier version deleted the entry alone and
// left the rest, on the reasoning that a dangling reference resolves to
// nothing and is therefore fail-closed. That is true of the REFERENCES and
// false of the STATE: because the vault, the pins and the approval records
// are all keyed by server id, re-adding that id later silently revived
// credentials, integrity baselines and remember-forever grants earned by a
// different server. A stale reference is inert; a stale grant is a live
// entitlement. References are now rewritten too — not because they were
// dangerous, but because leaving them made "removed" mean two different
// things depending on where you looked.
//
// Rewriting references is safe in one direction only, and every rewrite is
// checked against it: Profile.Servers is a three-state ALLOW list (nil = all,
// [] = none, [...] = that set — see registry.Profile), so dropping an id can
// only narrow the effective set. An emptied list stays [] and is never
// collapsed to nil, which would flip "none" into "all". No registry field
// carries exclusion semantics today; if one is ever added it must NOT be
// rewritten here.
//
// Deliberately kept: the audit, security and per-server trace logs. A log
// that forgot deleted servers would be worthless as evidence, and the removal
// itself is recorded in it.
//
// Failure direction: the registry transaction is committed FIRST, then the
// out-of-registry cleanups run and report failures as WARNINGS, never as
// errors. The alternatives are both worse — purging first would destroy
// credentials for a delete that then fails its precondition, and failing the
// whole operation on a keychain error would leave an operator unable to
// remove a server because the OS keychain was locked. The server is gone
// either way; each warning names exactly what survived and how to finish it
// by hand.
func RemoveServer(
	ctx context.Context, st *registry.Store, id string, pre Precondition, opts RemoveOptions,
) (ServerResult, error) {
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if _, ok := tx.Servers.V.Servers[id]; !ok {
			return serverNotFound(id)
		}
		delete(tx.Servers.V.Servers, id)
		forgetServerReferences(tx, id)
		return nil
	})
	if err != nil {
		return ServerResult{Result: res}, err
	}
	out := ServerResult{Result: res}
	if opts.Credentials != nil {
		out.Warnings = append(out.Warnings, purgeCredentials(ctx, opts.Credentials, id)...)
	}
	for _, f := range opts.State {
		if f == nil {
			continue
		}
		if err := f.ForgetServer(ctx, id); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"could not clear %s of %q (%v); it would apply again to a server re-added under this id",
				f.StateName(), id, err))
		}
	}
	return out, nil
}

// forgetServerReferences drops every mention of id from the registry
// documents other than servers.json. It runs INSIDE the delete transaction,
// so a server and its references can never be observed half-removed.
//
// Every rewrite here is narrowing-only; see RemoveServer's doc for why that
// property is what makes rewriting legitimate at all.
func forgetServerReferences(tx *registry.Tx, id string) {
	for name, doc := range tx.Profiles.V.Profiles {
		changed := false
		if i := slices.Index(doc.V.Servers, id); i >= 0 {
			// Allow list: deleting a member narrows. The result may be an
			// EMPTY non-nil slice, meaning "no servers" — exactly right, and
			// precisely why it must not be allowed to become nil.
			doc.V.Servers = slices.Delete(slices.Clone(doc.V.Servers), i, i+1)
			if doc.V.Servers == nil {
				doc.V.Servers = []string{}
			}
			changed = true
		}
		if _, ok := doc.V.Tools[id]; ok {
			delete(doc.V.Tools, id)
			changed = true
		}
		if changed {
			tx.Profiles.V.Profiles[name] = doc
		}
	}
	gov := &tx.Governance.V
	delete(gov.ResultBudget, id)
	// A rate-limit rule naming this server could only ever have restricted
	// it, so dropping the rule removes a quota that now matches nothing.
	// Rules with an empty or "*" server dimension are machine-wide and are
	// left alone.
	gov.RateLimits = slices.DeleteFunc(gov.RateLimits, func(r registry.Doc[registry.RateLimitRule]) bool {
		return r.V.Server == id
	})
}

// CredentialPurger deletes vault entries. It is the narrow face RemoveServer
// needs to make "remove a server" mean "and its credentials", and it is
// satisfied by *secrets.Chain.
//
// List is scope-blind on purpose: a server may hold credentials under scopes
// other than secrets.DefaultScope, and deleting only the two well-known keys
// in the default scope would leave those behind — the exact silent leftover
// this interface exists to prevent.
type CredentialPurger interface {
	List(ctx context.Context) ([]secrets.Ref, error)
	Delete(ctx context.Context, ref secrets.Ref) error
}

// StateForgetter drops one out-of-registry store's records for a server.
// Implemented by the integrity pin/approval stores, the approval allowlist
// and the gateway tool cache — each of which is keyed by server id and would
// otherwise pre-trust or pre-populate a server re-added under that id.
//
// StateName names the store in operator-facing warnings ("tool pins"), so a
// failed cleanup says what survived rather than leaking a file path.
//
// Contract: forgetting a server with no records is a no-op, never an error.
type StateForgetter interface {
	ForgetServer(ctx context.Context, serverID string) error
	StateName() string
}

// StateFunc adapts a plain cleanup function to StateForgetter, for stores
// exposed as a function rather than a handle (the gateway tool cache, the
// override store). name is what a warning calls it.
type StateFunc struct {
	Name   string
	Forget func(ctx context.Context, serverID string) error
}

// ForgetServer implements StateForgetter.
func (f StateFunc) ForgetServer(ctx context.Context, serverID string) error {
	if f.Forget == nil {
		return nil
	}
	return f.Forget(ctx, serverID)
}

// StateName implements StateForgetter.
func (f StateFunc) StateName() string { return f.Name }

// RemoveOptions tunes RemoveServer.
//
// There is deliberately no "keep credentials" switch. Removing a server means
// removing what it was entitled to; an operator who wants the definition gone
// but the tokens kept is describing `agenthub server disable`, which keeps
// both. The only reason a purge is skipped is a caller with no vault at all.
type RemoveOptions struct {
	// Credentials purges the removed server's vault entries. nil means the
	// caller has no vault wired — the purge is skipped and nothing is
	// reported, because there is no store to have missed anything in.
	Credentials CredentialPurger
	// State clears out-of-registry stores keyed by server id. Each entry is
	// independent: one failing store warns and the rest still run. nil
	// entries are skipped so callers can build the slice conditionally.
	State []StateForgetter
}

// purgeCredentials deletes every vault entry belonging to serverID, across
// all scopes and keys. It returns warnings rather than errors: see
// RemoveServer's failure direction.
//
// The unreadable-enc check is not an edge case: Set writes to secrets.enc
// whenever AGENTHUB_SECRET_KEY is set, while List can only see that file when
// the SAME key is present. Deleting from a shell without it would otherwise
// enumerate nothing, delete nothing, and report a clean purge over a
// surviving refresh token.
func purgeCredentials(ctx context.Context, v CredentialPurger, serverID string) []string {
	var warns []string
	if u, ok := v.(interface{ HasUnreadableEnc() bool }); ok && u.HasUnreadableEnc() {
		warns = append(warns, fmt.Sprintf(
			"credentials of %q in secrets.enc could not be read and may survive; "+
				"re-run with AGENTHUB_SECRET_KEY set, or 'agenthub auth logout %s'",
			serverID, serverID))
	}
	refs, err := v.List(ctx)
	if err != nil {
		return append(warns, fmt.Sprintf(
			"could not list credentials of %q (%v); run 'agenthub auth logout %s' to remove them",
			serverID, err, serverID))
	}
	for _, ref := range refs {
		if ref.ServerID != serverID {
			continue
		}
		if derr := v.Delete(ctx, ref); derr != nil {
			warns = append(warns, fmt.Sprintf(
				"could not remove credential %q of %q (%v); run 'agenthub auth logout %s'",
				ref.Key, serverID, derr, serverID))
		}
	}
	return warns
}

// SetServerEnabled flips a server's global enable flag. Disabling removes it
// from every profile's effective set without discarding its definition, so
// the switch is reversible and no configuration is lost.
func SetServerEnabled(
	ctx context.Context, st *registry.Store, id string, enabled bool, pre Precondition,
) (ServerResult, error) {
	var spec ServerSpec
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Servers.V.Servers[id]
		if !ok {
			return serverNotFound(id)
		}
		doc.V.Enabled = enabled
		tx.Servers.V.Servers[id] = doc
		spec = ServerSpec{ID: id, Entry: doc.V}
		return nil
	})
	if err != nil {
		return ServerResult{Result: res}, err
	}
	return ServerResult{Result: res, Servers: []ServerSpec{spec}}, nil
}

// serverNotFound is the shared "no such server" refusal. Every reference to
// a server id is checked against the registry so a typo becomes a refusal
// instead of a ghost entry that silently narrows nothing.
func serverNotFound(id string) *Error {
	e := notFoundf(CodeServerNotFound, "no server %q", id)
	e.Hint = "run 'agenthub server ls' to see configured servers"
	return e
}
