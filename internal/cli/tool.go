package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// `server tool ls` is the OFFLINE view of the aggregated catalog: it reads the
// registry (which servers are configured and enabled) plus the gateway's
// persisted tool cache, and never connects to anything. That is the same
// pair of inputs a cold gateway answers tools/list from (docs/flows.md), so
// the CLI shows what a client would see rather than a second opinion.
//
// --search reuses internal/discovery's ranker — the SAME scoring, tie-break
// and truncation the lazy-mode search_tools meta-tool uses. Two rankers
// would eventually disagree, and the one the operator debugs with must be
// the one the agent gets.
//
// THREE STATES, because listing only one of them was the bug this file used
// to have. It applied no allow list at all, so a server narrowed by an allow
// list still listed every tool it had ever cached: the one command that
// answers "what does this machine offer" answered a different question, and
// it was the only reader a rule with no other reader had. A tool is now `on`
// (offered), `blocked` (a rule exists and does not name it) or `pending`
// (the rule names it but no cached catalog has it — a typo, or a server no
// gateway has connected to yet).
//
// The allow list is applied by MERGING scope.ServerToolsLayer and handing the
// result to discovery.Visible, which screens through pipeline.ScopeAllows —
// the data plane's own predicate. Filtering the slice here instead would have
// been three lines and a second answer to a question that already has one.
//
// Blocked tools are held back by default and COUNTED in a footer rather than
// silently dropped; pending ones are always shown, because a misspelled rule
// has no other visible symptom. --all lists everything with the state spelled
// out in its own column.
const (
	toolStateOn      = "on"
	toolStateBlocked = "blocked"
	toolStatePending = "pending"
)

// descriptionColumnBytes bounds the description column of the human table.
// It is a byte bound on a rune boundary, like discovery's summary budget.
const descriptionColumnBytes = 72

// ToolRow is the per-tool data structure both output modes render from.
// Rank/Score are populated only by --search (rank 1 is the best match).
type ToolRow struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	RawName     string `json:"rawName"`
	Description string `json:"description,omitempty"`
	// State is the global allow list's verdict on this tool: on, blocked or
	// pending. It is omitted where nothing computed one — `server inspect
	// --tools` renders the same row type from one server's cache alone —
	// rather than defaulting to a verdict that was never reached.
	State string `json:"state,omitempty"`
	Rank  int    `json:"rank,omitempty"`
	Score int    `json:"score,omitempty"`
}

// ToolList is the `server tool ls` result. Ranked selects the search rendering,
// ShowAll the state column; the JSON shape is a plain array in every mode.
type ToolList struct {
	Rows   []ToolRow
	Ranked bool
	// ShowAll records that --all was given, which is what puts the STATE
	// column on the table. It is not derivable from the rows: a listing
	// where every tool happens to be `on` looks identical either way.
	ShowAll bool
	// Held is how many tools an allow list kept out of Rows. It is reported
	// even though the rows are gone, because a listing that quietly drops
	// part of its subject reads as a complete answer.
	Held int
}

// MarshalJSON emits the rows alone: the human/JSON split must not leak the
// rendering flag into the machine contract. An empty list marshals as [],
// never null.
func (l ToolList) MarshalJSON() ([]byte, error) {
	if l.Rows == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(l.Rows)
}

// Human renders the list as a table. The search rendering adds the RANK and
// SCORE columns and keeps the ranker's order; the plain rendering is sorted
// by exposed name.
func (l ToolList) Human(w io.Writer) error {
	if len(l.Rows) == 0 {
		switch {
		case l.Ranked:
			_, err := fmt.Fprintln(w, "no tool matches this query")
			return err
		case l.Held > 0:
			// Not "nothing cached": the tools are there and a rule is
			// holding all of them back, which is a different repair.
			_, err := fmt.Fprintf(w,
				"no tools offered: an allow list holds back all %d cached tool(s) "+
					"('--all' shows them, 'agenthub server tool allow <server> --all' drops the rule)\n", l.Held)
			return err
		default:
			_, err := fmt.Fprintln(w, "no tools cached yet (connect a client once so the gateway can populate the cache)")
			return err
		}
	}
	// Writes to a tabwriter only fail at Flush, which is where the error is
	// surfaced.
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	switch {
	case l.Ranked:
		_, _ = fmt.Fprintln(tw, "RANK\tSCORE\tNAME\tSERVER\tTOOL\tDESCRIPTION")
	case l.ShowAll:
		_, _ = fmt.Fprintln(tw, "STATE\tNAME\tSERVER\tTOOL\tDESCRIPTION")
	default:
		_, _ = fmt.Fprintln(tw, "NAME\tSERVER\tTOOL\tDESCRIPTION")
	}
	for _, r := range l.Rows {
		desc := oneLine(r.Description, descriptionColumnBytes)
		// A pending row has no exposed name to print: nothing routes to it,
		// and manufacturing one here would be exactly the guess RouteOf
		// exists to prevent. An empty cell reads as a rendering fault, so
		// the absence is spelled out.
		name := dashOr(r.Name, "-")
		switch {
		case l.Ranked:
			_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", r.Rank, r.Score, name, r.Server, r.RawName, desc)
		case l.ShowAll:
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.State, name, r.Server, r.RawName, desc)
		default:
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, r.Server, r.RawName, desc)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if l.Held > 0 {
		_, err := fmt.Fprintf(w, "\n%d tool(s) held back by an allow list; '--all' shows them\n", l.Held)
		return err
	}
	return nil
}

func (a *App) newToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tool",
		Aliases: []string{"tools"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "See and narrow the tools the servers contribute",
		Long: "What the servers on this machine offer, and how much of it they are\n" +
			"allowed to offer.\n\n" +
			"  server tool ls           what is offered right now\n" +
			"  server tool ls --rules   the allow lists themselves\n" +
			"  server tool inspect      one tool, and every layer's verdict on it\n" +
			"  server tool allow        change what a server offers, for every client\n\n" +
			"'agenthub profile tool allow' is the same edit one layer down, with the\n" +
			"same flags: this decides what the machine offers at all, that decides what\n" +
			"a profile passes on. The two intersect; neither can widen.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(a.newToolLsCmd())
	cmd.AddCommand(a.newToolInspectCmd())
	cmd.AddCommand(a.newToolAllowCmd())
	return cmd
}

func (a *App) newToolLsCmd() *cobra.Command {
	var (
		search  string
		showAll bool
		rules   bool
	)
	cmd := &cobra.Command{
		Use:   "ls [<server>]",
		Short: "List cached tools, optionally ranked by a search query",
		Long: "List the tools of the configured servers from the gateway's tool cache.\n\n" +
			"With --search the results are ranked by the same lexical ranker the\n" +
			fmt.Sprintf("lazy-mode search_tools meta-tool uses, best match first, capped at %d results.\n\n",
				discovery.MaxSearchLimit) +
			"What is listed is what this machine OFFERS: a tool an allow list holds back\n" +
			"is counted, not listed, and --all brings it back with the state of each.\n" +
			"--rules reads the allow lists themselves — do that before 'tool allow', which\n" +
			"replaces a rule rather than adding to it.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverArg := ""
			if len(args) == 1 {
				serverArg = args[0]
			}
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			if serverArg != "" {
				if _, ok := snap.Servers.V.Servers[serverArg]; !ok {
					e := NotFoundf(CodeServerNotFound, "no server %q", serverArg)
					e.Hint = "run 'agenthub server ls' to see configured servers"
					return e
				}
			}

			cached, err := gateway.LoadToolCache(a.resolver, nil)
			if err != nil {
				return err
			}
			if rules {
				return a.printer().Emit(
					toolRulesOf(snap.Servers.V.Servers, cached, serverArg), warnings...)
			}
			cat, err := offlineCatalogOf(snap, cached, serverArg)
			if err != nil {
				return err
			}

			tools := cat.visible
			if showAll {
				tools = append(append([]discovery.Tool{}, cat.visible...), cat.blocked...)
			}
			list, err := renderTools(surfaceOf(tools, snap.Generation), search)
			if err != nil {
				return err
			}
			list.ShowAll = showAll
			if !showAll {
				list.Held = len(cat.blocked)
			}
			list.mark(cat.blocked)
			// Ranking has nothing to rank a pending name against — there is
			// no description, no schema, no ToolDef at all — so it stays out
			// of --search rather than being scored against an empty string.
			if !list.Ranked {
				list.Rows = append(list.Rows, cat.pending...)
			}
			return a.printer().Emit(list, warnings...)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "rank the tools against a keyword query")
	cmd.Flags().BoolVar(&showAll, "all", false,
		"list the tools an allow list holds back too, with the state of each")
	cmd.Flags().BoolVar(&rules, "rules", false,
		"list the allow lists themselves, one row per server, instead of the tools")
	return cmd
}

// mark stamps the state column. Rows are `on` unless the blocked set names
// them, which is the fail-safe direction for a LABEL: a row wrongly marked
// blocked is a visible puzzle, one wrongly marked on is a lie about what a
// client can reach.
func (l *ToolList) mark(blocked []discovery.Tool) {
	out := make(map[string]bool, len(blocked))
	for _, t := range blocked {
		out[t.Exposed] = true
	}
	for i := range l.Rows {
		if out[l.Rows[i].Name] {
			l.Rows[i].State = toolStateBlocked
			continue
		}
		l.Rows[i].State = toolStateOn
	}
}

// offlineCatalog aggregates the cached tools of the ENABLED servers (only
// serverArg when given) under the same exposed-name rules the gateway uses,
// and splits them by the global allow list.
//
// The split is done by MERGING the layer, not by filtering the slice: a rule
// read straight off servers.json here would be a second implementation of
// what pipeline.ScopeAllows decides for every live call, and the two would
// eventually disagree about the case nobody tested. Only the CLIENT-specific
// layers are left out — there is no session to resolve them against — which
// is exactly what makes this "what the machine offers", the common prefix of
// what every client sees.
//
// The router is built from the FULL cached set before the split, because
// exposed names come from collision handling across the whole catalog: a
// router built from the surviving tools alone could hand out a different
// name than the gateway does, which is the one thing a debugging view must
// not do.
type offlineCatalog struct {
	visible []discovery.Tool
	blocked []discovery.Tool
	pending []ToolRow // named by a rule, absent from every cached catalog
}

func offlineCatalogOf(
	snap *registry.Snapshot,
	cached map[string][]mcp.ToolDef,
	serverArg string,
) (offlineCatalog, error) {
	servers := snap.Servers.V.Servers
	selected := make(map[string][]mcp.ToolDef, len(cached))
	for id, entry := range servers {
		if serverArg != "" && id != serverArg {
			continue
		}
		if !entry.V.Enabled {
			continue
		}
		if tools, ok := cached[id]; ok {
			selected[id] = tools
		}
	}
	rt, err := router.BuildFromCache(selected)
	if err != nil {
		return offlineCatalog{}, fmt.Errorf("aggregate cached tools: %w", err)
	}

	all := discovery.Visible(rt, nil)
	out := offlineCatalog{visible: all}
	layer, ok := scope.ServerToolsLayer(snap)
	if !ok {
		return out, nil // no server carries a rule: everything is offered
	}
	// The catalog scope intersects against is keyed by RAW tool names, which
	// is why it is rebuilt from the cache rather than read off the router:
	// the router speaks exposed names, and seeding an intersection with those
	// would silently drop every renamed tool.
	raw := make(map[string][]string, len(selected))
	for id, defs := range selected {
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		raw[id] = names
	}
	eff, err := scope.Merge([]scope.ScopeLayer{layer}, router.NewCatalog(raw))
	if err != nil {
		return offlineCatalog{}, fmt.Errorf("resolve the global allow list: %w", err)
	}
	kept := discovery.Visible(rt, eff)
	live := make(map[string]bool, len(kept))
	for _, t := range kept {
		live[t.Exposed] = true
	}
	out.visible = kept
	for _, t := range all {
		if !live[t.Exposed] {
			out.blocked = append(out.blocked, t)
		}
	}

	// Pending: a rule naming a tool no cached catalog has. It cannot come
	// out of the router — there is no ToolDef to route to — so it is
	// synthesized from the rule itself. A server with no cached catalog at
	// all is skipped: every one of its names would be reported, and "no
	// gateway has connected yet" is not a spelling mistake.
	for _, id := range sortedKeys(servers) {
		if serverArg != "" && id != serverArg {
			continue
		}
		if !servers[id].V.Enabled {
			continue
		}
		for _, name := range unknownRuleNames(servers[id].V.Tools, selected[id]) {
			out.pending = append(out.pending, ToolRow{
				Server: id, RawName: name, State: toolStatePending,
				Description: "named by the allow list; no cached catalog has it",
			})
		}
	}
	return out, nil
}

// unknownRuleNames names the entries of an allow list that no cached tool
// matches — the spelling mistakes, and the only lasting symptom they have.
//
// An EMPTY catalog yields nothing, deliberately: every name would be
// reported, and a server no gateway has connected to yet is not a typo. That
// is the same silence unknownToolWarning keeps at write time, for the same
// reason, and the two must not disagree about it.
func unknownRuleNames(rule []string, defs []mcp.ToolDef) []string {
	if len(rule) == 0 || len(defs) == 0 {
		return nil
	}
	known := make(map[string]bool, len(defs))
	for _, d := range defs {
		known[d.Name] = true
	}
	var out []string
	for _, name := range rule {
		if !known[name] {
			out = append(out, name)
		}
	}
	return out
}

// surfaceOf wraps a tool set in the full-mode discovery surface --search
// ranks against.
func surfaceOf(tools []discovery.Tool, generation uint64) *discovery.Surface {
	return discovery.New(discovery.Options{
		Mode:       discovery.ModeFull,
		Tools:      tools,
		Generation: generation,
	})
}

// renderTools turns a surface into the rendered list, ranking it when a
// query is given.
func renderTools(s *discovery.Surface, query string) (ToolList, error) {
	if strings.TrimSpace(query) == "" {
		tools := s.Tools() // sorted by exposed name
		rows := make([]ToolRow, 0, len(tools))
		for _, t := range tools {
			rows = append(rows, rowForTool(t))
		}
		return ToolList{Rows: rows}, nil
	}

	// nil guard: the SearchGuard breaks an AGENT's search loop and is per
	// session; a CLI invocation is neither.
	res, err := s.Search(discovery.SearchRequest{Query: query, Limit: discovery.MaxSearchLimit}, nil)
	if err != nil {
		var de *discovery.Error
		if errors.As(err, &de) {
			// The ranker's own validation message is the contract; reusing it
			// keeps the CLI and the meta-tool saying the same thing.
			return ToolList{}, Usagef("--search: %s", de.Message)
		}
		return ToolList{}, err
	}
	rows := make([]ToolRow, 0, len(res.Hits))
	for _, h := range res.Hits {
		t, ok := s.Lookup(h.Tool)
		if !ok {
			continue // unreachable: hits are drawn from this very surface
		}
		row := rowForTool(t)
		row.Rank, row.Score = h.Rank, h.Score
		rows = append(rows, row)
	}
	return ToolList{Rows: rows, Ranked: true}, nil
}

func rowForTool(t discovery.Tool) ToolRow {
	return ToolRow{
		Name:        t.Exposed,
		Server:      t.ServerID,
		RawName:     t.RawTool,
		Description: t.Def.Description,
	}
}

// oneLine collapses whitespace runs into single spaces and truncates to max
// bytes on a rune boundary, marking a cut with a single-rune ellipsis. It
// keeps a hostile (or merely verbose) multi-line description from wrecking
// the table without changing what --json reports.
func oneLine(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= max {
		return out
	}
	const ellipsis = "…" // 3 bytes, counted INSIDE the bound
	limit := max - len(ellipsis)
	if limit <= 0 {
		return ellipsis
	}
	cut := 0
	for i := range out {
		if i > limit {
			break
		}
		cut = i
	}
	return out[:cut] + ellipsis
}
