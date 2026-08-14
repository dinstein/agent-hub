package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/registry"
)

// sourceManual marks entries created interactively via the CLI (flags or
// pasted stdin JSON), as opposed to a catalog entry's "catalog:<id>".
const sourceManual = "manual"

// ServerRow is the per-server data structure both output modes render from.
//
// Headers are rendered with their VALUES INTACT only because a registry
// entry never holds a credential: values are ${SECRET_X} placeholders that
// name a vault entry (docs/subsystems/cli.md rule 5 — no CLI surface ever echoes a
// secret; resolution happens at connect time inside internal/downstream).
type ServerRow struct {
	ID        string            `json:"id"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// OAuth mirrors the entry's login hints. NeedsAuth is deliberately
	// absent: it is runtime state (a live 401), never configuration.
	OAuth      *registry.OAuthHint `json:"oauth,omitempty"`
	Provenance string              `json:"provenance,omitempty"`
	// Runtime is omitted for the host default so `--json` output of a
	// pre-M2 entry is byte-identical to what it always was.
	Runtime string                  `json:"runtime,omitempty"`
	Docker  *registry.DockerRuntime `json:"docker,omitempty"`
	// Derive is the derived-instance policy. Omitted for the shared default,
	// which is also what an entry written before the field existed means —
	// same byte-identical grounds as Runtime.
	Derive  string `json:"derive,omitempty"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source,omitempty"`
	// Trace mirrors the entry's frame-log switch, omitted when off so the
	// --json output of an untraced entry is byte-identical to what it always
	// was.
	Trace bool `json:"trace,omitempty"`
	// Auth is the credential this MACHINE holds for the server, filled in by
	// `server ls` and `server inspect` alone — `server add` and `catalog add`
	// leave it nil, both because their --json output stays byte-identical and
	// because an entry that was written a microsecond ago has no credential
	// story worth telling.
	//
	// It does not contradict the NeedsAuth ban above: this is what is STORED
	// here, readable offline, while needsAuth would be a claim about what the
	// SERVER will accept — a claim only a live 401 can make, and the one that
	// goes stale the moment a token expires.
	Auth *ServerAuth `json:"auth,omitempty"`
	// ToolRule is the entry's global tool allow list, read against the cached
	// catalog (serverrule.go). Like Auth it is filled in by `server ls` and
	// `server inspect` alone — the rule is on the entry either way, but its
	// count and spelling check need the tool cache, which `server add` has no
	// business loading and whose --json output stays byte-identical without it.
	ToolRule *ServerToolRule `json:"tool_rule,omitempty"`
}

// target is the connection target column: the command line for stdio, the
// endpoint URL for the HTTP transports. A containerized entry names its
// image first — "where does this run" is the question the column answers,
// and for a docker-runtime server the command alone answers it wrongly.
func (r ServerRow) target() string {
	if r.URL != "" {
		return r.URL
	}
	line := strings.TrimSpace(strings.Join(append([]string{r.Command}, r.Args...), " "))
	if r.Runtime == registry.RuntimeDocker && r.Docker != nil {
		return strings.TrimSpace("docker[" + r.Docker.Image + "] " + line)
	}
	return line
}

// ServerList is the `server ls` result. JSON shape: a plain array.
type ServerList []ServerRow

// Human renders the list as a table.
func (l ServerList) Human(w io.Writer) error {
	if len(l) == 0 {
		_, err := fmt.Fprintln(w, "no servers configured")
		return err
	}
	// Writes to a tabwriter only fail at Flush, which is where the error is
	// surfaced.
	//
	// Both middle columns appear only when they have something to say, for one
	// reason: a column that reads "off" — or "-" — on every row for the rest
	// of time teaches readers to stop seeing it, which is the opposite of the
	// point. TRACE is a temporary debugging state that writes raw payloads to
	// disk, so it has to be visible in the listing an operator already reads.
	// AUTH is absent on a machine whose servers are all local subprocesses,
	// where "no credential" is not news; the moment one server has a
	// credential, what every OTHER row lacks becomes information too.
	//
	// TOOLS follows the same rule for the same reason, and it is where the
	// global allow list is read now: it appears as soon as one server narrows
	// its tools, and on a machine where none does it says nothing anyone has
	// to learn to skip.
	traced, credentialed, narrowed := false, false, false
	for _, r := range l {
		traced = traced || r.Trace
		credentialed = credentialed || (r.Auth != nil && r.Auth.Kind != authKindNone)
		narrowed = narrowed || (r.ToolRule != nil && r.ToolRule.narrows())
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	head := []string{"ID", "TRANSPORT", "ENABLED"}
	if credentialed {
		head = append(head, "AUTH")
	}
	if narrowed {
		head = append(head, "TOOLS")
	}
	if traced {
		head = append(head, "TRACE")
	}
	head = append(head, "SOURCE", "TARGET")
	_, _ = fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, r := range l {
		cells := []string{r.ID, r.Transport, strconv.FormatBool(r.Enabled)}
		if credentialed {
			cells = append(cells, r.Auth.cell())
		}
		if narrowed {
			cells = append(cells, r.ToolRule.cell())
		}
		if traced {
			cells = append(cells, traceMark(r.Trace))
		}
		cells = append(cells, r.Source, r.target())
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := l.writeRuleHint(w); err != nil {
		return err
	}
	return l.writeAuthHints(w)
}

// writeRuleHint explains the "!" the TOOLS column can carry. It names the
// servers rather than the count: the repair is per server, and the reader has
// to open one of them to find out which name is wrong.
func (l ServerList) writeRuleHint(w io.Writer) error {
	var ids []string
	for _, r := range l {
		if r.ToolRule != nil && len(r.ToolRule.Unknown) > 0 {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w,
		"\n! an allow list names a tool no cached catalog has, so it lets nothing through: %s\n"+
			"  'agenthub server inspect %s' spells the names out\n",
		strings.Join(ids, ", "), ids[0])
	return err
}

// authHintLimit bounds the footer. Past a handful, a list of repairs stops
// being a prompt to act and becomes a second table to read.
const authHintLimit = 3

// writeAuthHints prints the repair for every row that needs one: the AUTH
// cell says WHICH servers are in trouble, and these lines say what to run.
// Rows that are fine contribute nothing, so a healthy machine prints no
// footer at all.
func (l ServerList) writeAuthHints(w io.Writer) error {
	var hints []string
	for _, r := range l {
		if h := r.Auth.hintText(); h != "" {
			hints = append(hints, h)
		}
	}
	if len(hints) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, h := range hints[:min(len(hints), authHintLimit)] {
		if _, err := fmt.Fprintln(w, h); err != nil {
			return err
		}
	}
	if extra := len(hints) - authHintLimit; extra > 0 {
		// Never silently truncated: the count is the difference between "these
		// are the problems" and "these are three of the problems".
		_, err := fmt.Fprintf(w, "(+%d more; 'agenthub auth status' lists them all)\n", extra)
		return err
	}
	return nil
}

// traceMark renders the TRACE cell. "on" is spelled out and off is a dash,
// so the eye lands on the rows that are recording rather than on the rest.
func traceMark(on bool) string {
	if on {
		return "on"
	}
	return "-"
}

// AddedServers is the `server add` result.
type AddedServers struct {
	Added []ServerRow `json:"added"`
}

// Human renders one "added:" line per entry (docs/subsystems/cli.md example),
// then names the step that puts the server into service. An entry nobody is
// told to enable is indistinguishable from one that was never added.
func (a AddedServers) Human(w io.Writer) error {
	for _, r := range a.Added {
		if _, err := fmt.Fprintf(w, "added: %s (%s, source=%s, disabled)\n", r.ID, r.Transport, r.Source); err != nil {
			return err
		}
	}
	for _, r := range a.Added {
		if _, err := fmt.Fprintf(w, "next: agenthub server enable %s\n", r.ID); err != nil {
			return err
		}
	}
	return nil
}

// RemovedServer is the `server rm` result.
type RemovedServer struct {
	Removed string `json:"removed"`
}

// Human renders the removal confirmation.
func (r RemovedServer) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "removed: %s\n", r.Removed)
	return err
}

func (a *App) newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"servers"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Add, switch on and inspect the MCP servers agenthub connects to",
		Long: "Servers are registered once here, for every client at the same time. 'add'\n" +
			"writes the definition and leaves it off; 'enable' connects and puts it into\n" +
			"service.\n\n" +
			"Enabled servers are the most any client can ever see, so 'disable' takes one\n" +
			"away from everybody at once and no profile can put it back.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(
		a.newServerLsCmd(), a.newServerInspectCmd(), a.newServerAddCmd(), a.newServerRmCmd(),
		a.newServerToggleCmd(true), a.newServerToggleCmd(false),
		a.newServerTestCmd(), a.newServerTraceCmd(), a.newServerLogsCmd(),
		// `tool` lives HERE, not at the top level, because the rule it
		// writes lives on the server entry beside `enabled` and answers the
		// same question: what this machine offers at all. It was the one
		// place where the storage and the command tree disagreed, and the
		// cost was not tidiness — a top-level group withheld from the
		// release help page meant the global allow list was shipped with no
		// advertised way to read or write it.
		a.newToolCmd(),
	)
	return cmd
}

func (a *App) newServerAddCmd() *cobra.Command {
	var (
		cmdPath      string
		argv         []string
		envKV        []string
		cwd          string
		url          string
		transportOpt string
		headerKV     []string
		local        bool
		fromStdin    bool
		docker       dockerFlags
		oauth        oauthFlags
	)
	// `add` writes the definition and nothing else: no connection, no probe,
	// no enable. The entry lands DISABLED and `server enable` is the second,
	// separate step (which is where the probe lives).
	//
	// Two operations rather than one because they answer different
	// questions. `add` records what a server IS — pure configuration, no
	// network, deterministic, safe to script against a downstream that is
	// currently unreachable. `enable` declares the operator wants to USE it,
	// which is the only point at which "can we actually reach it?" is worth
	// asking. Folding the two together made `enabled` mean both "the user
	// wants this" and "it answered a probe", and a downstream that was
	// merely deploying at add time then looked like a server that had never
	// been added.
	//
	// Callers that want the one-shot experience compose the two: the GUI
	// does exactly that, and `catalog add` names the enable in its
	// NextSteps. Composition belongs to the caller; the primitives stay
	// separate.
	cmd := &cobra.Command{
		Use:   "add [<name>]",
		Short: "Add an MCP server, switched off, from flags or from JSON you paste in",
		Long: "Use --cmd for a server agenthub launches locally, --url for one behind an HTTP\n" +
			"endpoint, or --stdin to pipe in the JSON block a README gives you. (--runtime\n" +
			"docker fails outright rather than falling back to your machine.)\n\n" +
			"This only writes the definition down: nothing connects, and the server stays\n" +
			"off until 'agenthub server enable <id>' probes it and puts it into service.\n\n" +
			"Never put a real token in --env or --header: write ${SECRET_NAME} and the\n" +
			"value is looked up from agenthub's credential store at startup.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			var entries []namedEntry
			if fromStdin {
				if cmdPath != "" || len(argv) > 0 || len(envKV) > 0 || cwd != "" ||
					url != "" || transportOpt != "" || len(headerKV) > 0 || local ||
					docker.any() || oauth.any() {
					e := Usagef("--stdin cannot be combined with the definition flags")
					e.Hint = helpHint(cmd)
					return e
				}
				data, err := io.ReadAll(a.stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				entries, err = normalizeStdin(data, name)
				if err != nil {
					return err
				}
			} else {
				if name == "" {
					e := Usagef("a server name argument is required (or use --stdin)")
					e.Hint = helpHint(cmd)
					return e
				}
				entry, err := entryFromFlags(cmd, addFlags{
					command:   cmdPath,
					args:      argv,
					env:       envKV,
					cwd:       cwd,
					url:       url,
					transport: transportOpt,
					headers:   headerKV,
					local:     local,
					docker:    docker,
					oauth:     oauth,
				})
				if err != nil {
					return err
				}
				entries = []namedEntry{{Name: name, Entry: entry}}
			}

			// Validate BEFORE the store is opened: a rejected entry must not
			// leave a half-written registry behind, and the operator can
			// still fix what they typed. confops re-checks under the lock —
			// this call only moves the refusal earlier.
			specs := make([]confops.ServerSpec, 0, len(entries))
			for _, ne := range entries {
				spec := confops.ServerSpec{ID: ne.Name, Entry: ne.Entry}
				if err := confops.ValidateServerSpec(spec); err != nil {
					return opsError(err)
				}
				specs = append(specs, spec)
			}

			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.AddServers(cmd.Context(), store, specs, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}

			result := AddedServers{Added: make([]ServerRow, 0, len(res.Servers))}
			for _, spec := range res.Servers {
				result.Added = append(result.Added, rowFor(spec.ID, spec.Entry))
			}
			return a.printer().Emit(result, warnings...)
		},
	}
	cmd.Flags().StringVar(&cmdPath, "cmd", "", "program agenthub runs to start the server, e.g. npx")
	cmd.Flags().StringSliceVar(&argv, "args", nil, "arguments for that program, comma-separated")
	cmd.Flags().StringArrayVar(&envKV, "env", nil, "environment variable KEY=VALUE (repeatable); write ${SECRET_X} instead of a real secret")
	cmd.Flags().StringVar(&cwd, "cwd", "",
		"directory the server is started in; for a container that is a path inside it, same as --container-workdir")
	cmd.Flags().StringVar(&url, "url", "", "address of a server that already runs behind HTTP")
	cmd.Flags().StringVar(&transportOpt, "transport", "", "how to reach it: stdio (assumed with --cmd), http or sse (assumed with --url)")
	cmd.Flags().StringArrayVar(&headerKV, "header", nil, "HTTP header KEY=VALUE (repeatable); write ${SECRET_X} instead of a real secret")
	cmd.Flags().BoolVar(&local, "local", false,
		"allow a --url pointing at this machine (localhost); other private addresses stay refused")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false,
		`read the server's JSON from standard input ({"mcpServers":{...}} or one bare entry)`)
	cmd.Flags().StringVar(&docker.runtime, "runtime", "",
		"run the server on this machine (host, the default) or inside a container (docker)")
	cmd.Flags().StringVar(&docker.image, "image", "",
		"container image to run; setting this turns on --runtime docker")
	cmd.Flags().StringArrayVar(&docker.mounts, "mount", nil,
		"directory of yours the container may see: SRC[:DST][:ro|rw] (repeatable; read-only unless you say rw)")
	cmd.Flags().StringVar(&docker.network, "network", "",
		"container network; by default the container gets no network at all")
	cmd.Flags().StringVar(&docker.memory, "memory", "", "how much memory the container may use, e.g. 512m")
	cmd.Flags().StringVar(&docker.cpus, "cpus", "", "how many CPUs the container may use, e.g. 1.5")
	cmd.Flags().StringVar(&docker.user, "container-user", "", "user the container runs as")
	cmd.Flags().StringVar(&docker.workdir, "container-workdir", "",
		"directory the container starts in; wins over --cwd when both are given")
	cmd.Flags().StringArrayVar(&docker.extra, "docker-arg", nil,
		"extra argument for 'docker run' (repeatable); it may not undo the container's restrictions")
	cmd.Flags().StringVar(&oauth.issuer, "oauth-issuer", "",
		"tell agenthub which login service to use, instead of asking the server")
	cmd.Flags().StringSliceVar(&oauth.scopes, "oauth-scope", nil,
		"permission to ask for at login (repeatable or comma-separated); sent exactly as written")
	cmd.Flags().StringVar(&oauth.resourceMetadata, "oauth-resource-metadata", "",
		"address of the server's login-details document, if it is not where agenthub would look")
	return cmd
}

// oauthFlags is the login-hint half of `server add`. The three fields are
// the whole of registry.OAuthHint: what a later `agenthub auth login` should
// discover against. They are transport-independent (an stdio child may proxy
// to a remote authorization server), so they are not part of either
// transport's flag group.
//
// needsAuth is deliberately absent and stays absent: whether a server
// currently requires authorization is runtime state, discovered by a live
// 401, never configuration.
type oauthFlags struct {
	issuer           string
	scopes           []string
	resourceMetadata string
}

func (f oauthFlags) any() bool {
	return f.issuer != "" || len(f.scopes) > 0 || f.resourceMetadata != ""
}

// hint builds the registry hint, or nil when no flag was given. nil rather
// than an empty struct: an entry that was never given hints must not grow an
// empty "oauth": {} block on disk, because that is a difference a diff shows
// and a reader has to explain.
func (f oauthFlags) hint() *registry.OAuthHint {
	if !f.any() {
		return nil
	}
	return &registry.OAuthHint{
		Issuer:              strings.TrimSpace(f.issuer),
		Scopes:              f.scopes,
		ResourceMetadataURL: strings.TrimSpace(f.resourceMetadata),
	}
}

// addFlags is the flag half of `server add`, grouped so the (already long)
// RunE keeps one statement per concern.
type addFlags struct {
	command   string
	args      []string
	env       []string
	cwd       string
	url       string
	transport string
	headers   []string
	local     bool
	docker    dockerFlags
	oauth     oauthFlags
}

// entryFromFlags builds one registry entry from the definition flags,
// inferring the transport from which of --cmd / --url is present and
// rejecting every combination that would silently ignore a flag.
func entryFromFlags(cmd *cobra.Command, f addFlags) (registry.ServerEntry, error) {
	var zero registry.ServerEntry
	usage := func(format string, a ...any) error {
		e := Usagef(format, a...)
		e.Hint = helpHint(cmd)
		return e
	}

	kind := f.transport
	switch {
	case kind == "":
		// Inference, not magic: exactly one of the two target flags decides.
		switch {
		case f.url != "" && f.command != "":
			return zero, usage("--cmd and --url are mutually exclusive; pick one transport")
		case f.url != "":
			kind = registry.TransportHTTP
		case f.command != "":
			kind = registry.TransportStdio
		default:
			return zero, usage("--cmd or --url is required (or use --stdin)")
		}
	case kind != registry.TransportStdio && kind != registry.TransportHTTP && kind != registry.TransportSSE:
		return zero, &Error{
			Code: CodeUnsupportedTransport, ExitCode: ExitUsage,
			Message: fmt.Sprintf("unknown transport %q", kind),
			Hint:    "supported transports: stdio, http (streamable-http), sse (legacy HTTP+SSE)",
		}
	}

	// Enabled is false: `add` writes configuration and touches nothing else
	// (see the newServerAddCmd header). `server enable` is the step that
	// probes and puts the server into service.
	entry := registry.ServerEntry{Transport: kind, Enabled: false, Source: sourceManual}
	// Login hints are transport-independent — attached before the transport
	// split so an stdio entry that proxies to a remote authorization server
	// keeps them too, exactly like the pasted-JSON path does.
	entry.OAuth = f.oauth.hint()
	if kind == registry.TransportStdio {
		if f.command == "" {
			return zero, usage("the stdio transport needs --cmd")
		}
		if f.url != "" || len(f.headers) > 0 || f.local {
			return zero, usage("--url/--header/--local apply to the http and sse transports only")
		}
		env, err := parseKVFlags("--env", f.env)
		if err != nil {
			return zero, err
		}
		entry.Command = f.command
		entry.Args = f.args
		entry.Env = env
		entry.Cwd = f.cwd
		if err := applyDockerFlags(&entry, f.docker, usage); err != nil {
			return zero, err
		}
		return entry, nil
	}
	if f.docker.any() {
		return zero, usage("--runtime/--image and the container flags apply to the stdio transport only")
	}

	if f.url == "" {
		return zero, usage("the %s transport needs --url", kind)
	}
	if f.command != "" || len(f.args) > 0 || len(f.env) > 0 || f.cwd != "" {
		return zero, usage("--cmd/--args/--env/--cwd apply to the stdio transport only")
	}
	headers, err := parseKVFlags("--header", f.headers)
	if err != nil {
		return zero, err
	}
	if err := validateEndpoint(f.url, f.local); err != nil {
		return zero, err
	}
	entry.URL = f.url
	entry.Headers = headers
	if f.local {
		entry.Provenance = registry.ProvenanceLocal
	}
	return entry, nil
}

// validateEndpoint screens an endpoint at the moment the operator can still
// fix it. The predicate and its narrow --local exception live in confops so
// `server add --url`, pasted stdin JSON and the control plane all refuse the
// same set of addresses.
func validateEndpoint(raw string, local bool) error {
	return opsError(confops.ValidateEndpoint(raw, local))
}

func (a *App) newServerLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every server you have added, and whether it is switched on",
		Long: "This is what is registered, not what is working: a server listed here can\n" +
			"still fail to start. 'agenthub server test <id>' answers that.\n\n" +
			"The AUTH column reports the credential stored on THIS machine — not whether\n" +
			"the server accepts it, which only a real call can tell you. TOOLS is the\n" +
			"allow list set by 'agenthub server tool allow', and appears only once some\n" +
			"server carries one; 'agenthub server tool ls' lists what that leaves offered.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			// The cached catalog is an enrichment too: it supplies the count
			// the rule is read against and the spelling check, and neither is
			// worth failing a listing of the registry over.
			cached, cacheErr := gateway.LoadToolCache(a.resolver, nil)
			if cacheErr != nil {
				warnings = append(warnings, "tool cache unreadable: "+cacheErr.Error())
			}
			// The credential state is an enrichment, never a precondition: its
			// failures arrive as warnings and error cells, and the registry
			// half of the listing is printed either way.
			probe, probeWarnings := a.newAuthProbe(cmd.Context())
			warnings = append(warnings, probeWarnings...)
			now := time.Now()

			snap := store.Snapshot()
			rows := make(ServerList, 0, len(snap.Servers.V.Servers))
			for name, doc := range snap.Servers.V.Servers {
				row := rowFor(name, doc.V)
				auth := probe.classify(cmd.Context(), name, doc.V, now)
				row.Auth = &auth
				rule := serverToolRuleOf(doc.V, cached[name])
				row.ToolRule = &rule
				rows = append(rows, row)
			}
			slices.SortFunc(rows, func(x, y ServerRow) int { return strings.Compare(x.ID, y.ID) })
			return a.printer().Emit(rows, warnings...)
		},
	}
}

func (a *App) newServerRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a server and everything stored for it",
		Long: "Permanent, and it takes the whole footprint with it: credentials,\n" +
			"profile membership and tool rules, governance rules naming it, and\n" +
			"the cached tool list. Its log file is kept.\n\n" +
			"To stop a server being used without losing any of that, use\n" +
			"'agenthub server disable' instead.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			deps, derr := a.newOAuthDeps(false)
			if derr != nil {
				return derr
			}
			// State cleanups are best-effort by construction: each store that
			// cannot be opened simply contributes nothing, and RemoveServer
			// reports it as a warning naming what survived. They are built
			// unconditionally, exactly as the daemon builds them, because the
			// two front ends have to agree on what deleting a server means.
			opts := confops.RemoveOptions{
				Credentials: deps.chain,
				State:       a.serverStateForgetters(),
			}
			res, err := confops.RemoveServer(cmd.Context(), store, id, noPrecondition, opts)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(RemovedServer{Removed: id}, warnings...)
		},
	}
}

// serverStateForgetters builds the out-of-registry cleanups `server rm`
// runs. A store that cannot even be OPENED is skipped rather than failing the
// command: the registry entry is already gone by the time these run, and
// RemoveServer reports whatever fails as a warning naming what survived.
func (a *App) serverStateForgetters() []confops.StateForgetter {
	var out []confops.StateForgetter
	out = append(out,
		confops.StateFunc{
			Name: "the cached tool list",
			Forget: func(_ context.Context, id string) error {
				return gateway.ForgetToolCache(a.resolver, id)
			},
		},
	)
	return out
}

func rowFor(name string, e registry.ServerEntry) ServerRow {
	return ServerRow{
		ID:         name,
		Transport:  e.TransportName(),
		Command:    e.Command,
		Args:       e.Args,
		Env:        e.Env,
		Cwd:        e.Cwd,
		URL:        e.URL,
		Headers:    e.Headers,
		OAuth:      e.OAuth,
		Provenance: e.Provenance,
		Runtime:    e.Runtime,
		Docker:     e.Docker,
		Derive:     e.Derive,
		Enabled:    e.Enabled,
		Source:     e.Source,
		Trace:      e.Trace,
	}
}

// parseKVFlags parses repeated KEY=VALUE flags (--env, --header). flag is
// the flag name, used verbatim in the error so the message names what the
// operator actually typed.
func parseKVFlags(flag string, kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, Usagef("%s expects KEY=VALUE, got %q", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

// namedEntry pairs a server name with its normalized registry entry.
type namedEntry struct {
	Name  string
	Entry registry.ServerEntry
}

// normalizeStdin turns pasted JSON into registry entries.
//
// It reads the document with internal/catalog — Recognize for the shape,
// MapEntry for each entry — which is the SAME reader the GUI's paste preview
// uses, with the one deliberate difference passed in as an argument:
// RejectUnknownFields. A key agenthub does not model, or a key whose value has
// a type it cannot read, is an ERROR here rather than a warning, because this
// path writes with no preview in front of it; `server add` would otherwise
// report success while the "oauth" block the user pasted is simply absent
// (docs/subsystems/cli.md).
//
// This used to be a second reader — a struct with DisallowUnknownFields and a
// literal "mcpServers" check — and it drifted from the first in every
// direction the type system does not police. It modelled no "disabled" /
// "enabled" key, so a Cline or Zed snippet was a hard error here and fine in
// the GUI; it knew one wrapper key, so a VS Code {"mcp":{"servers":…}}
// fragment failed with a per-entry complaint about a server named "mcp"; and
// it read only "url", not the "serverUrl" / "httpUrl" spellings. The transport
// table had already been shared for the same reason one drift earlier (see
// catalog.TransportFromSpelling); this is the rest of it.
// TestPasteRoutesAgreeOnWholeSnippets drives whole documents through both
// routes and compares the entries, so a private copy of any of it fails there
// rather than in somebody's paste.
//
// What stays this route's own, because it is policy rather than reading:
//
//   - the entry lands DISABLED whatever the pasted configuration says, exactly
//     as the flags path does. `add` records what a server IS; `server enable`
//     is the separate step that probes it and puts it into service. Nothing is
//     lost in silence — the command prints "added: <name> (…, disabled)".
//   - Source is "manual": an operator typed this, whatever it was copied from.
//   - agenthub's own gateway entry — present in every adapted client
//     configuration — is REFUSED rather than skipped: a write has nowhere to
//     report a skip.
//   - a remote endpoint is screened with no --local escape hatch: a snippet
//     copied from a README must not be able to point the connector at a
//     private address (fail-closed; `server add --url --local` is the
//     explicit, typed-by-a-human path).
//
// A name argument may rename the entry only when exactly one entry is present,
// and is REQUIRED for the single-entry shape, which names nothing. Results are
// sorted by name for deterministic output.
func normalizeStdin(data []byte, nameArg string) ([]namedEntry, error) {
	invalid := func(format string, a ...any) *Error {
		return &Error{Code: CodeInvalidJSON, ExitCode: ExitGeneral, Message: fmt.Sprintf(format, a...)}
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, invalid("stdin is empty; expected server JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, invalid("stdin is not a JSON object: %v", err)
	}
	shape, _, rawEntries, ok := catalog.Recognize(top)
	if !ok {
		return nil, invalid("stdin does not look like an MCP server configuration; "+
			"expected a wrapper key (%s), a name -> entry object, or a single entry "+
			"with a command or url", strings.Join(catalog.SectionNames(), ", "))
	}
	if shape == catalog.ShapeSingleEntry && nameArg == "" {
		return nil, Usagef("a server name argument is required when stdin holds a single entry")
	}
	if len(rawEntries) == 0 {
		return nil, invalid("no server entries found in stdin JSON")
	}
	if nameArg != "" {
		if len(rawEntries) > 1 {
			return nil, Usagef("name argument %q conflicts with %d entries in stdin JSON", nameArg, len(rawEntries))
		}
		// Exactly one entry: the name argument renames it.
		for _, v := range rawEntries {
			rawEntries = map[string]json.RawMessage{nameArg: v}
			break
		}
	}

	entries := make([]namedEntry, 0, len(rawEntries))
	for name, raw := range rawEntries {
		if strings.TrimSpace(name) == "" {
			return nil, invalid("server entries must have a non-empty name")
		}
		entry, _, err := catalog.MapEntry(raw, catalog.RejectUnknownFields)
		if err != nil {
			return nil, stdinEntryError(name, err)
		}
		if reason, self := catalog.IsGatewayEntry(entry); self {
			// The preview skips this one; a write has nowhere to report a
			// skip, so it refuses the whole paste instead. Adding agenthub's
			// own gateway entry points it at itself, and the regress presents
			// as a hang rather than as an error.
			return nil, invalid("server %q: %s", name, reason)
		}
		entry.Enabled = false
		entry.Source = sourceManual
		if entry.Transport != registry.TransportStdio {
			if err := validateEndpoint(entry.URL, false); err != nil {
				return nil, err
			}
		}
		entries = append(entries, namedEntry{Name: name, Entry: entry})
	}
	slices.SortFunc(entries, func(x, y namedEntry) int { return strings.Compare(x.Name, y.Name) })
	return entries, nil
}

// stdinEntryError translates a catalog mapping refusal into this command's
// vocabulary. It switches on the KIND rather than on the message, so the two
// routes may word the same refusal differently without the exit code moving.
func stdinEntryError(name string, err error) error {
	var mapErr *catalog.EntryError
	if errors.As(err, &mapErr) && mapErr.Kind == catalog.EntryUnknownTransport {
		return &Error{
			Code: CodeUnsupportedTransport, ExitCode: ExitGeneral,
			Message: fmt.Sprintf("server %q: %s", name, mapErr.Message),
			Hint:    "supported transports: stdio, http (streamable-http), sse (legacy HTTP+SSE)",
		}
	}
	return &Error{
		Code: CodeInvalidJSON, ExitCode: ExitGeneral,
		Message: fmt.Sprintf("server %q: %v", name, err),
	}
}
