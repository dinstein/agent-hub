package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
)

// `server enable|disable|inspect` complete the server group.
//
// enable/disable flip registry.ServerEntry.Enabled, which removes the
// server from every profile's effective set at once (docs/modules/controlplane.md) — the
// global switch, above any per-profile narrowing.
//
// inspect is offline-capable and reads the SAME cache `tool ls` reads: the
// operator's view of what a cold gateway would answer. When the daemon is
// up its live state is added on top; when it is not, the report says so
// rather than pretending the cache is a live handshake.

// ServerToggle is the `server enable|disable` result.
type ServerToggle struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Changed bool   `json:"changed"`
	// Probe is the `enable` reachability check. Absent on disable, on
	// --no-probe, and for entries the CLI cannot dial (docker). It is
	// descriptive only: the enable happened regardless of what it says.
	Probe *ProbeResult `json:"probe,omitempty"`
}

// ProbeResult is one connection attempt made on behalf of `server enable`.
// Reachable and NeedsAuth are distinct because they call for different
// next steps, and neither is an error: the server IS enabled either way.
type ProbeResult struct {
	Reachable bool `json:"reachable"`
	// NeedsAuth is a live 401, never configuration (the same distinction
	// ServerRow documents about its own absent NeedsAuth field).
	NeedsAuth bool   `json:"needsAuth,omitempty"`
	Tools     int    `json:"tools,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Human renders the toggle, then whatever the probe found.
func (t ServerToggle) Human(w io.Writer) error {
	state := "disabled"
	if t.Enabled {
		state = "enabled"
	}
	if !t.Changed {
		if _, err := fmt.Fprintf(w, "%s already %s\n", t.ID, state); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(w, "%s: %s\n", t.ID, state); err != nil {
		return err
	}
	switch {
	case t.Probe == nil:
		return nil
	case t.Probe.Reachable:
		_, err := fmt.Fprintf(w, "  reachable, %d tool(s)\n", t.Probe.Tools)
		return err
	case t.Probe.NeedsAuth:
		_, err := fmt.Fprintf(w, "  needs authorization: run 'agenthub auth login %s'\n", t.ID)
		return err
	default:
		_, err := fmt.Fprintf(w, "  not reachable right now: %s\n", oneLine(t.Probe.Detail, descriptionColumnBytes))
		return err
	}
}

// ServerInspect is the `server inspect` result.
//
// It is the only command that describes ONE server completely, so it is
// where facts otherwise spread across a dozen listings are joined up: what
// the entry says, what this machine holds for it, and what a running gateway
// currently makes of it. Everything but the live block is readable with the
// daemon down, and the live block's absence is stated rather than papered
// over with cache figures presented as runtime.
type ServerInspect struct {
	Server ServerRow `json:"server"`
	// Live carries daemon-observed runtime state; nil when the daemon is
	// offline (the report never invents runtime facts).
	Live *ServerLive `json:"live,omitempty"`
	// Tools is the cached catalog, present with --tools.
	Tools []ToolRow `json:"tools,omitempty"`
	// Schema is one tool's raw input schema, present with --schema.
	Schema json.RawMessage `json:"schema,omitempty"`
	// ToolCount is the number of cached tools, always reported.
	ToolCount int `json:"tool_count"`
	// CacheOnly marks that Tools/ToolCount came from the persisted cache
	// rather than a live handshake.
	CacheOnly bool `json:"cache_only"`
	// CachedAt is when that cache was written. Zero means no gateway has
	// ever connected to this server — the distinction a bare "0 tools"
	// cannot draw, and the one that decides whether the answer is "this
	// server offers nothing" or "nobody has ever asked it".
	CachedAt time.Time `json:"cached_at,omitzero"`
	// Secrets lists the ${SECRET_X} placeholders this entry needs, so the
	// operator can cross-check them against `agenthub secret ls`. Values
	// are never resolved here.
	Secrets []string `json:"secret_refs,omitempty"`
	// TraceLog is where the recorded frames land, present only while
	// tracing is on. It is printed because a trace nobody can find is a
	// trace nobody reads — and because that file holds UNREDACTED payloads,
	// which deserves to be named on the same line that says the switch is
	// on.
	TraceLog string `json:"trace_log,omitempty"`
	// DockerRun is the exact command line the spawner would run for a
	// containerized entry. "Isolation a config claims must be delivered" is
	// verified by reading it, and no other command prints it.
	DockerRun []string `json:"docker_run,omitempty"`
}

// ServerLive is the daemon's view of one server.
type ServerLive struct {
	State      string `json:"state"`
	Tools      int    `json:"tools"`
	Health     string `json:"health"`
	AdminState string `json:"admin_state"`
	Summary    string `json:"summary"`
	Action     string `json:"action,omitempty"`
}

// Human renders the detail view as titled sections of label/value pairs.
//
// THE SHAPE IS THE POINT. A server is described by four independent things,
// and a flat list of lines makes a reader classify each one for themselves:
// how it is CONFIGURED, what credential this machine HOLDS, who may SEE it,
// and what is true of it right now. Those are the sections, in that order —
// static to live, and cheapest-to-answer first — and a section prints only
// when it has something to say, so a plain local subprocess still fits in a
// few lines rather than growing four headings of dashes.
func (i ServerInspect) Human(w io.Writer) error {
	r := i.Server
	d := &detailWriter{w: w}
	d.line("%s (%s, source=%s, enabled=%s)", r.ID, r.Transport, dash(r.Source), boolText(r.Enabled))

	d.section("configuration")
	d.field("target", "%s", r.target())
	if r.Cwd != "" {
		d.field("cwd", "%s", r.Cwd)
	}
	i.writeRuntime(d)
	// Provenance is reported only when it is the exception, because that is
	// the only state worth a line: ProvenanceLocal is an operator-declared
	// exemption from SSRF screening, and an entry carrying one should not
	// have to be read out of the JSON envelope to find that out.
	if r.Provenance == registry.ProvenanceLocal {
		d.field("endpoint", "declared local — a loopback address is allowed for this entry")
	}
	if mode := downstream.DeriveMode(r.Derive); mode != "" && mode != downstream.DeriveNone {
		d.field("derive", "%s (%s)", r.Derive, deriveText(mode))
	}
	if r.Trace {
		d.field("trace", "on -> %s", dash(i.TraceLog))
		d.cont("frames are recorded BEFORE redaction; 'agenthub server logs %s' reads them", r.ID)
	}
	i.writeEnvAndHeaders(d)

	i.writeCredentials(d)
	i.writeStatus(d)
	i.writeCatalog(d)
	return d.err
}

// writeRuntime renders where the server actually runs. For a containerized
// entry that is not a detail: the run line IS the isolation, and it is
// rendered by the same translator the spawn guard screens, so what is
// printed here is what would be executed.
func (i ServerInspect) writeRuntime(d *detailWriter) {
	docker := i.Server.Docker
	if i.Server.Runtime != registry.RuntimeDocker || docker == nil {
		return
	}
	limits := []string{"image=" + dash(docker.Image), "network=" + dashOr(docker.Network, "none")}
	for _, kv := range [][2]string{
		{"memory", docker.Memory}, {"cpus", docker.CPUs},
		{"user", docker.User}, {"workdir", docker.Workdir},
	} {
		if kv[1] != "" {
			limits = append(limits, kv[0]+"="+kv[1])
		}
	}
	d.field("runtime", "docker  %s", strings.Join(limits, "  "))
	for n, m := range docker.Mounts {
		access := "ro"
		if m.Write {
			access = "rw"
		}
		d.at(n, "mounts", "%s -> %s (%s)", m.Source, dashOr(m.Target, m.Source), access)
	}
	if len(docker.Mounts) == 0 {
		d.field("mounts", "none — the container sees no host path")
	}
	if len(i.DockerRun) > 0 {
		// The argv carries no binary name — the spawn guard screens the shape
		// with the literal command "docker" beside it — so the human line
		// puts it back. What is printed has to be pasteable, or an operator
		// checking the isolation by running it themselves is debugging the
		// rendering instead.
		d.field("spawns", "docker %s", strings.Join(i.DockerRun, " "))
	}
}

// writeEnvAndHeaders renders the values that reach the downstream. They are
// part of the configuration, not of the credential state: what a placeholder
// RESOLVES to is the credentials section's business, and this one only shows
// that the flag landed where the operator put it.
func (i ServerInspect) writeEnvAndHeaders(d *detailWriter) {
	for n, k := range sortedKeys(i.Server.Env) {
		d.at(n, "env", "%s=%s", k, i.Server.Env[k])
	}
	for n, k := range sortedKeys(i.Server.Headers) {
		d.at(n, "headers", "%s: %s", k, headerValueText(k, i.Server.Headers[k]))
	}
}

// writeCredentials renders what the configuration EXPECTS (the login hints,
// the placeholders) beside what this machine actually HOLDS. The two are
// deliberately adjacent: nearly every "it worked yesterday" report is one of
// them having moved without the other.
func (i ServerInspect) writeCredentials(d *detailWriter) {
	r := i.Server
	auth := r.Auth != nil && r.Auth.Kind != authKindNone
	if !auth && r.OAuth == nil && len(i.Secrets) == 0 {
		return
	}
	d.section("credentials")
	if auth {
		d.field("auth", "%s", r.Auth.line())
		if h := r.Auth.hint(r.ID); h != "" {
			d.cont("%s", h)
		}
	}
	// The login hints are configuration, not a credential: rendering them is
	// how an operator confirms `server add --oauth-issuer` landed without
	// having to read the JSON envelope.
	if oa := r.OAuth; oa != nil {
		d.field("oauth", "issuer=%s  scopes=%s  resource-metadata=%s",
			dash(oa.Issuer), dash(strings.Join(oa.Scopes, " ")), dash(oa.ResourceMetadataURL))
	}
	if len(i.Secrets) > 0 && !i.authLineCoversSecrets() {
		d.field("secrets", "%s", strings.Join(i.secretStates(), ", "))
		d.cont("values are never shown; 'agenthub secret set %s <KEY>' stores one", r.ID)
	}
}

// authLineCoversSecrets reports that the credential line above has already
// named every required key and what is wrong with it. The per-key line earns
// its space when the two answers differ — three keys of which one is missing
// — and merely repeats the sentence above it when they do not.
func (i ServerInspect) authLineCoversSecrets() bool {
	a := i.Server.Auth
	if a == nil || a.Kind != authKindSecret || a.State != authStateMissing {
		return false
	}
	return len(a.MissingSecrets) == len(i.Secrets)
}

// secretStates marks each required vault key stored or missing. The state
// comes from the SAME classification the auth line above it renders — the
// ladder's first rung reports every missing key — so the two cannot end up
// disagreeing about the same server. A vault that could not be read yields
// neither answer, and says so instead of guessing "stored".
func (i ServerInspect) secretStates() []string {
	unknown := i.Server.Auth == nil || i.Server.Auth.Kind == authKindUnknown
	missing := map[string]bool{}
	if i.Server.Auth != nil {
		for _, k := range i.Server.Auth.MissingSecrets {
			missing[k] = true
		}
	}
	out := make([]string, 0, len(i.Secrets))
	for _, key := range i.Secrets {
		switch {
		case unknown:
			out = append(out, key+" (unreadable vault)")
		case missing[key]:
			out = append(out, key+" (MISSING)")
		default:
			out = append(out, key+" (stored)")
		}
	}
	return out
}

// writeStatus renders the two answers about NOW, and keeps them apart. The
// live line is a daemon's observation; the cached line is a file on this
// disk. Presenting a cache figure where a reader expects runtime is how a
// server that has been down for a week looks like one with twelve tools.
func (i ServerInspect) writeStatus(d *detailWriter) {
	d.section("status")
	if i.Live != nil {
		d.field("live", "%s, health %s/%s, %d tool(s)",
			i.Live.State, i.Live.Health, i.Live.AdminState, i.Live.Tools)
		if i.Live.Summary != "" {
			d.cont("%s", i.Live.Summary)
		}
		// The daemon's suggested repair was carried in the struct but never
		// printed: the one field of a health report that says what to DO.
		if i.Live.Action != "" {
			d.cont("next: %s", i.Live.Action)
		}
	} else {
		d.field("live", "daemon offline — everything above is registry and cache only")
	}
	d.field("cached", "%s", i.cachedText())
}

// cachedText describes the persisted catalog, never as a live figure.
func (i ServerInspect) cachedText() string {
	if i.CachedAt.IsZero() {
		return "no catalog stored — no gateway has connected to this server yet"
	}
	return fmt.Sprintf("%d tool(s), recorded %s (%s)",
		i.ToolCount, humanAge(time.Since(i.CachedAt)), i.CachedAt.Local().Format(time.RFC3339))
}

// writeCatalog renders what --schema and --tools asked for.
func (i ServerInspect) writeCatalog(d *detailWriter) {
	if len(i.Schema) > 0 {
		d.section("schema")
		d.raw(indentJSON(i.Schema) + "\n")
	}
	if len(i.Tools) == 0 {
		return
	}
	d.section("cached tools")
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  TOOL\tDESCRIPTION")
	for _, t := range i.Tools {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", t.RawName, oneLine(t.Description, descriptionColumnBytes))
	}
	if err := tw.Flush(); err != nil {
		d.fail(err)
		return
	}
	d.raw(sb.String())
}

// deriveText spells out what a derivation mode costs, because the word alone
// ("session") does not say that it multiplies processes.
func deriveText(mode downstream.DeriveMode) string {
	switch mode {
	case downstream.DeriveRoot:
		return "one instance per project root"
	case downstream.DeriveSession:
		return "one instance per session"
	default:
		// Not a mode this build knows: the connection will REFUSE it rather
		// than fall back to shared state, so it is reported as the error it
		// will become instead of being quietly normalized here.
		return "unknown mode — the connection will be refused"
	}
}

// headerValueText renders a header value for the HUMAN view.
//
// Header values are printed verbatim because a registry entry is supposed to
// hold `${SECRET_X}` placeholders rather than credentials, and seeing which
// placeholder landed in which header is the whole point of the line. The one
// exception is the case where that assumption is already broken: a LITERAL
// Authorization value is a pasted token, and inspect will not read it back
// out to a terminal. The test is the same narrow one `hasLiteralAuthorization`
// makes — any other header may or may not authenticate anything, and guessing
// would start hiding ordinary configuration.
//
// The --json envelope is deliberately unchanged: it carries the entry as
// stored, for programs that already have the file it came from.
func headerValueText(name, value string) string {
	if strings.EqualFold(name, "authorization") && strings.TrimSpace(value) != "" &&
		len(downstream.SecretKeysIn(value)) == 0 {
		return "<literal value, not shown — store it as a secret and use ${SECRET_X}>"
	}
	return value
}

// newServerToggleCmd builds `server enable` / `server disable`.
//
// `enable` is where the connection probe lives, because it is the step that
// declares the operator wants to USE the server (`add` only records what it
// is). The probe REPORTS, it does not veto: the enable is what was asked
// for and it always happens. A server that needs a login is enabled and
// says so — refusing would strand an entry the user explicitly enabled, and
// a downstream that is merely down right now must not become a
// configuration change.
func (a *App) newServerToggleCmd(enable bool) *cobra.Command {
	verb, short := "disable", "Switch a server off for every client at once"
	long := "Takes the server away from every client at once, and no profile can bring it\n" +
		"back. The server and its credentials stay; 'agenthub server enable' restores it."
	if enable {
		verb, short = "enable", "Switch a server on, connecting to it first to see what it needs"
		long = "Connects to the server before putting it into service, so a missing credential\n" +
			"or a broken command is reported here rather than inside someone's AI client.\n\n" +
			"A client bound to a profile that leaves this server out still will not see it;\n" +
			"check with 'agenthub profile ls'."
	}
	var noProbe bool
	cmd := &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Long:  long,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetServerEnabled(cmd.Context(), store, id, enable, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			out := ServerToggle{ID: id, Enabled: enable, Changed: res.Changed}
			if enable && !noProbe {
				out.Probe = a.probeForEnable(cmd.Context(), id, a.printer())
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	if enable {
		cmd.Flags().BoolVar(&noProbe, "no-probe", false,
			"enable without connecting first (skips the reachability and login check)")
	}
	return cmd
}

// ServerTrace is the `server trace` result.
type ServerTrace struct {
	ID      string `json:"id"`
	Trace   bool   `json:"trace"`
	Changed bool   `json:"changed"`
	// Path is where the frames land, printed even when switching off so the
	// operator knows which file still holds what was already captured.
	Path string `json:"path"`
}

// Human renders the switch and the file behind it.
func (s ServerTrace) Human(w io.Writer) error {
	state := "off"
	if s.Trace {
		state = "on"
	}
	if _, err := fmt.Fprintf(w, "%s: trace %s\n  %s\n", s.ID, state, s.Path); err != nil {
		return err
	}
	if !s.Trace {
		return nil
	}
	_, err := fmt.Fprintf(w, "  frames are recorded before redaction; read them with "+
		"'agenthub server logs %s'\n", s.ID)
	return err
}

// newServerTraceCmd builds `server trace <id> on|off`.
//
// It is its own command rather than a flag on enable/disable because it
// answers a different question — enable decides whether a server is IN
// SERVICE, trace decides whether its wire is recorded — and folding them
// would make one of the two invisible in the other's help text.
//
// The warning about redaction is in the command's own Long text, not only in
// the docs: this is the one switch in the CLI that writes downstream results
// to disk verbatim, and a person turning it on is exactly the person who has
// not read the module documentation.
func (a *App) newServerTraceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trace <id> <on|off>",
		Short: "Record the JSON-RPC frames exchanged with one server, for debugging",
		Long: "Writes every request and response between agenthub and this server to\n" +
			"<data>/logs/server-<id>.log, which 'agenthub server logs' reads back.\n\n" +
			"Frames are captured at the connection, BEFORE any redaction, so the file\n" +
			"holds whatever the server actually returned — turn it on to diagnose\n" +
			"something, and off again afterwards. It is off by default, per server, and\n" +
			"a running client picks the change up without being restarted.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			var on bool
			switch args[1] {
			case "on":
				on = true
			case "off":
				on = false
			default:
				e := Usagef("trace takes 'on' or 'off', not %q", args[1])
				e.Hint = "see 'agenthub server trace --help'"
				return e
			}
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetServerTrace(cmd.Context(), store, id, on, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			logsDir, err := a.resolver.LogsDir()
			if err != nil {
				return err
			}
			return a.printer().Emit(ServerTrace{
				ID: id, Trace: on, Changed: res.Changed,
				Path: downstream.ServerLogPath(logsDir, id),
			}, warnings...)
		},
	}
}

// probeForEnable connects to id once and classifies the outcome. It never
// returns an error: every result is descriptive, because the enable it
// accompanies has already happened.
//
// Docker entries are probed like any other: the dial spawns the container,
// not the host command (see dockerruntime.go).
func (a *App) probeForEnable(ctx context.Context, id string, p *output.Printer) *ProbeResult {
	entry, _, err := a.serverEntry(id)
	if err != nil {
		return nil
	}
	spec, err := downstream.SpecFromEntry(id, entry)
	if err != nil {
		return nil
	}
	deps, err := a.newOAuthDeps(entry.Provenance == registry.ProvenanceLocal)
	if err != nil {
		return nil
	}
	ddeps := downstream.Deps{Secrets: deps.chain.Resolver()}
	if spec.IsHTTP() {
		ddeps.Auth = deps.tokenSource(id)
	}
	p.Progress(output.ProgressEvent{
		Event:   "probing",
		Message: fmt.Sprintf("checking %s…", id),
		Fields:  map[string]any{"server": id},
	})
	srv, err := downstream.Connect(ctx, spec, ddeps)
	if err == nil {
		defer srv.Close()
		return &ProbeResult{Reachable: true, Tools: len(srv.Tools())}
	}
	// By status, not by substring — the same reasoning as testConnectError:
	// a proxy's 502 whose body quotes an upstream 401 must not be reported
	// as "you need to log in".
	if transport.IsAuthStatus(err) {
		return &ProbeResult{NeedsAuth: true, Detail: err.Error()}
	}
	return &ProbeResult{Detail: err.Error()}
}

func (a *App) newServerInspectCmd() *cobra.Command {
	var (
		withTools bool
		schema    string
	)
	cmd := &cobra.Command{
		Use:   "inspect <id> [--tools] [--schema <tool>]",
		Short: "Show one server's settings, its known tools and whether it is currently up",
		Long: "The tool list shown is remembered from the last contact, not fetched now. To\n" +
			"connect for real and see what the server answers today, use\n" +
			"'agenthub server test <id>'.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			entry, err := requireServer(snap, id)
			if err != nil {
				return err
			}
			cached, err := gateway.LoadToolCacheEntries(a.resolver, nil)
			if err != nil {
				return err
			}
			defs := cached[id].Tools
			row := rowFor(id, entry)
			// One server, so the probe's index-first economy buys nothing here
			// — but sharing the classification with `server ls` is the point:
			// two commands describing the same credential must not be able to
			// describe it differently.
			probe, probeWarnings := a.newAuthProbe(cmd.Context())
			warnings = append(warnings, probeWarnings...)
			auth := probe.classify(cmd.Context(), id, entry, time.Now())
			row.Auth = &auth
			out := ServerInspect{
				Server:    row,
				ToolCount: len(defs),
				CacheOnly: true,
				CachedAt:  cached[id].SavedAt,
				Secrets:   secretRefsOf(entry),
			}
			// The two facts that live outside the registry document but
			// describe THIS entry. Neither may take the report down: a
			// missing logs directory or a container config that no longer
			// renders is worth a warning, and every other line is still
			// true (the same fail-open direction the credential probe takes).
			if entry.Trace {
				logsDir, lerr := a.resolver.LogsDir()
				if lerr != nil {
					warnings = append(warnings, "could not resolve the log directory: "+lerr.Error())
				} else {
					out.TraceLog = downstream.ServerLogPath(logsDir, id)
				}
			}
			if entry.Runtime == registry.RuntimeDocker {
				line, derr := dockerRunLine(id, entry)
				if derr != nil {
					warnings = append(warnings, fmt.Sprintf(
						"the container configuration does not render into a 'docker run' line (%v); "+
							"connecting to %s will fail for the same reason", derr, id))
				}
				out.DockerRun = line
			}
			if schema != "" {
				def, ok := findTool(defs, schema)
				if !ok {
					e := NotFoundf(CodeToolNotFound, "server %q has no cached tool %q", id, schema)
					e.Hint = "run 'agenthub server inspect " + id + " --tools' to see the cached catalog"
					return e
				}
				out.Schema = def.InputSchema
			}
			if withTools {
				out.Tools = make([]ToolRow, 0, len(defs))
				for _, d := range defs {
					out.Tools = append(out.Tools, ToolRow{
						Name: d.Name, Server: id, RawName: d.Name, Description: d.Description,
					})
				}
			}
			if live, ok := a.liveServerState(cmd, id); ok {
				out.Live = live
				out.CacheOnly = false
			} else {
				warnings = append(warnings,
					"daemon offline: tool catalog and counts come from the persisted cache, not a live handshake")
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().BoolVar(&withTools, "tools", false, "list the tools recorded for this server")
	cmd.Flags().StringVar(&schema, "schema", "", "print the argument definition of one recorded tool")
	return cmd
}

// liveServerState fetches the daemon's view of one server. Failure is not
// an error: inspect must still work with no daemon.
func (a *App) liveServerState(cmd *cobra.Command, id string) (*ServerLive, bool) {
	ctl, _, err := a.requireDaemon(cmd.Context())
	if err != nil {
		return nil, false
	}
	var servers []struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Tools  int    `json:"tools"`
		Health struct {
			Level      string `json:"level"`
			AdminState string `json:"admin_state"`
			Summary    string `json:"summary"`
			Action     string `json:"action"`
		} `json:"health"`
	}
	if err := ctl.do(cmd.Context(), "GET", "/v1/servers", nil, &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.ID != id {
			continue
		}
		return &ServerLive{
			State: s.State, Tools: s.Tools, Health: s.Health.Level,
			AdminState: s.Health.AdminState, Summary: s.Health.Summary, Action: s.Health.Action,
		}, true
	}
	return nil, false
}

func findTool(defs []mcp.ToolDef, name string) (mcp.ToolDef, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return mcp.ToolDef{}, false
}

// secretRefsOf collects the vault KEYS an entry needs, in the exact spelling
// `agenthub secret set` writes and `agenthub secret ls` prints. Only keys are
// collected: the registry stores placeholders verbatim and resolution happens
// at connect time, so no value can pass through here.
//
// The extraction is downstream.SecretKeysIn rather than a local ${...} scan,
// because the two rules that turn a placeholder into a CREDENTIAL belong to
// the resolver: only ${SECRET_<KEY>} is looked up in the vault, and the entry
// it names is <KEY> WITHOUT the prefix. A scan that ignored both produced a
// list that failed at exactly the cross-check it exists for — it offered
// ${ROOT} as a secret to go and store, and asked for "SECRET_GITHUB_TOKEN"
// when the stored key is "GITHUB_TOKEN".
func secretRefsOf(e registry.ServerEntry) []string {
	seen := map[string]bool{}
	collect := func(m map[string]string) {
		for _, v := range m {
			for _, key := range downstream.SecretKeysIn(v) {
				seen[key] = true
			}
		}
	}
	collect(e.Env)
	collect(e.Headers)
	return sortedKeys(seen)
}
