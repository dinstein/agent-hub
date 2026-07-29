package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

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
	// Secrets lists the ${SECRET_X} placeholders this entry needs, so the
	// operator can cross-check them against `agenthub secret ls`. Values
	// are never resolved here.
	Secrets []string `json:"secret_refs,omitempty"`
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

// Human renders the detail view.
func (i ServerInspect) Human(w io.Writer) error {
	r := i.Server
	if _, err := fmt.Fprintf(w, "%s (%s, source=%s, enabled=%s)\n",
		r.ID, r.Transport, dash(r.Source), boolText(r.Enabled)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "target: %s\n", r.target()); err != nil {
		return err
	}
	if r.Cwd != "" {
		if _, err := fmt.Fprintf(w, "cwd: %s\n", r.Cwd); err != nil {
			return err
		}
	}
	// The login hints are configuration, not a credential: rendering them is
	// how an operator confirms `server add --oauth-issuer` landed without
	// having to read the JSON envelope.
	if oa := r.OAuth; oa != nil {
		if _, err := fmt.Fprintf(w, "oauth: issuer=%s scopes=%s resource-metadata=%s\n",
			dash(oa.Issuer), dash(strings.Join(oa.Scopes, " ")), dash(oa.ResourceMetadataURL)); err != nil {
			return err
		}
	}
	if len(i.Secrets) > 0 {
		if _, err := fmt.Fprintf(w, "secret refs: %v (values are never shown)\n", i.Secrets); err != nil {
			return err
		}
	}
	if i.Live != nil {
		if _, err := fmt.Fprintf(w, "live: state=%s health=%s/%s tools=%d %s\n",
			i.Live.State, i.Live.Health, i.Live.AdminState, i.Live.Tools, i.Live.Summary); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "live: daemon offline (report is registry + tool cache only)"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "cached tools: %d\n", i.ToolCount); err != nil {
		return err
	}
	if len(i.Schema) > 0 {
		_, err := fmt.Fprintf(w, "%s\n", i.Schema)
		return err
	}
	if len(i.Tools) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TOOL\tDESCRIPTION")
	for _, t := range i.Tools {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", t.RawName, oneLine(t.Description, descriptionColumnBytes))
	}
	return tw.Flush()
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

// probeForEnable connects to id once and classifies the outcome. It never
// returns an error: every result is descriptive, because the enable it
// accompanies has already happened.
//
// Docker entries are skipped rather than guessed at — the CLI has no
// container probe wired (errDockerProbeUnwired), and reporting "unreachable"
// for something never dialed would be a lie.
func (a *App) probeForEnable(ctx context.Context, id string, p *output.Printer) *ProbeResult {
	entry, _, err := a.serverEntry(id)
	if err != nil || entry.IsDocker() {
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
			cached, err := gateway.LoadToolCache(a.resolver, nil)
			if err != nil {
				return err
			}
			defs := cached[id]
			out := ServerInspect{
				Server:    rowFor(id, entry),
				ToolCount: len(defs),
				CacheOnly: true,
				Secrets:   secretRefsOf(entry),
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

// secretRefsOf collects the ${SECRET_X} placeholder NAMES an entry uses.
// Only names are collected: the registry stores placeholders verbatim and
// resolution happens at connect time, so no value can pass through here.
func secretRefsOf(e registry.ServerEntry) []string {
	seen := map[string]bool{}
	collect := func(m map[string]string) {
		for _, v := range m {
			for _, ref := range secretPlaceholders(v) {
				seen[ref] = true
			}
		}
	}
	collect(e.Env)
	collect(e.Headers)
	return sortedKeys(seen)
}

// secretPlaceholders extracts ${NAME} occurrences from a value.
func secretPlaceholders(v string) []string {
	var out []string
	for i := 0; i+1 < len(v); i++ {
		if v[i] != '$' || v[i+1] != '{' {
			continue
		}
		end := -1
		for j := i + 2; j < len(v); j++ {
			if v[j] == '}' {
				end = j
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, v[i+2:end])
		i = end
	}
	return out
}
