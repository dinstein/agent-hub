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
	// BlockedBy names the layer that took a blocked tool away: global, or
	// which half of a profile. It is set only on a blocked or pending row —
	// on an offered one there is no layer to name, and printing the last one
	// consulted would read as one that decided something.
	BlockedBy string `json:"blockedBy,omitempty"`
	Rank      int    `json:"rank,omitempty"`
	Score     int    `json:"score,omitempty"`
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
	// Layered records that more than one layer could have blocked a row,
	// which is what puts the BY column on the table. Under the global layer
	// alone every blocked row would read "global", and a column with one
	// value in it is not an answer.
	Layered bool
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
		case l.Held > 0 && l.Layered:
			// Which layer took them is exactly what the reader needs and
			// cannot be said here — there is one line and several possible
			// answers, one per row — so it points at the listing that says.
			_, err := fmt.Fprintf(w,
				"nothing gets through: all %d cached tool(s) are held back "+
					"('--all' shows them, each with the layer that took it)\n", l.Held)
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
	// BY rides with STATE: it says which layer produced the state, so on its
	// own it would be a column of reasons for verdicts the table is not
	// showing.
	withBy := l.ShowAll && l.Layered
	switch {
	case l.Ranked:
		_, _ = fmt.Fprintln(tw, "RANK\tSCORE\tNAME\tSERVER\tTOOL\tDESCRIPTION")
	case withBy:
		_, _ = fmt.Fprintln(tw, "STATE\tBY\tNAME\tSERVER\tTOOL\tDESCRIPTION")
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
		case withBy:
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.State, dashOr(r.BlockedBy, "-"), name, r.Server, r.RawName, desc)
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
		// The layered listing does not name the rule: with more than one layer
		// able to have taken them, naming one would be a guess about rows that
		// are not on the table.
		reason := " by an allow list"
		if l.Layered {
			reason = ""
		}
		_, err := fmt.Fprintf(w, "\n%d tool(s) held back%s; '--all' shows them\n", l.Held, reason)
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
			"  server tool ls        what is offered right now\n" +
			"  server tool inspect   one tool, and every layer's verdict on it\n" +
			"  server tool allow     change what a server offers, for every client\n\n" +
			"The rules themselves are on the servers that hold them: 'agenthub server ls'\n" +
			"has a TOOLS column and 'agenthub server inspect <server>' spells one out.\n\n" +
			"'agenthub profile tool' is the same three commands one layer down, with the\n" +
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
			"is counted, not listed, and --all brings it back with the state of each.\n\n" +
			"This is the EFFECT of the rules. The rules themselves are on the servers that\n" +
			"carry them — 'agenthub server ls' has a TOOLS column and 'agenthub server\n" +
			"inspect <server>' spells one out. Read them before 'tool allow', which\n" +
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
				if _, err := requireServer(snap, serverArg); err != nil {
					return err
				}
			}

			cached, err := gateway.LoadToolCache(a.resolver, nil)
			if err != nil {
				return err
			}
			if rules {
				// Advisory, and on stderr: the root has SetOut(stdout), so a
				// notice printed there would corrupt --json for exactly the
				// scripted callers the flag is being kept alive for. It is
				// also advisory in the other sense — a stderr that will not
				// take the note must not turn a working command into a
				// failure, which is why the write is discarded.
				_, _ = fmt.Fprint(a.stderr,
					"note: the tool rules are now reported by 'agenthub server ls' and "+
						"'agenthub server inspect <server>'; --rules still works but will be removed\n")
				return a.printer().Emit(
					toolRulesOf(snap.Servers.V.Servers, cached, serverArg), warnings...)
			}
			list, err := toolListing(snap, cached, toolListingRequest{
				server: serverArg, search: search, showAll: showAll,
			})
			if err != nil {
				return err
			}
			return a.printer().Emit(list, warnings...)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "rank the tools against a keyword query")
	cmd.Flags().BoolVar(&showAll, "all", false,
		"list the tools an allow list holds back too, with the state of each")
	cmd.Flags().BoolVar(&rules, "rules", false,
		"deprecated: the rules are now on 'server ls' and 'server inspect'")
	// Hidden rather than removed, for one release. It is kept out of --help
	// because help is what a reader consults INSTEAD of running the command,
	// and a documented flag reads as the way to do this rather than as the
	// way it used to be done.
	_ = cmd.Flags().MarkHidden("rules")
	return cmd
}

// toolListingRequest is what the two listings differ by. profile is the only
// field either of them sets alone, which is the point: `server tool ls` and
// `profile tool ls` are one rendering of one catalog at two altitudes, and a
// second implementation is how the two would come to disagree about a tool.
type toolListingRequest struct {
	server  string
	profile string
	search  string
	showAll bool
}

// toolListing builds the rendered list for either altitude.
func toolListing(
	snap *registry.Snapshot,
	cached map[string][]mcp.ToolDef,
	req toolListingRequest,
) (ToolList, error) {
	cat, err := offlineCatalogOf(snap, cached, req.server, req.profile)
	if err != nil {
		return ToolList{}, err
	}
	tools := cat.visible
	if req.showAll {
		tools = append(append([]discovery.Tool{}, cat.visible...), cat.blocked...)
	}
	list, err := renderTools(surfaceOf(tools, snap.Generation), req.search)
	if err != nil {
		return ToolList{}, err
	}
	list.ShowAll = req.showAll
	list.Layered = req.profile != ""
	if !req.showAll {
		list.Held = len(cat.blocked)
	}
	list.mark(cat)
	// Ranking has nothing to rank a pending name against — there is no
	// description, no schema, no ToolDef at all — so it stays out of --search
	// rather than being scored against an empty string.
	if !list.Ranked {
		list.Rows = append(list.Rows, cat.pending...)
	}
	return list, nil
}

// mark stamps the state column, and for a blocked row the layer that took it.
// Rows are `on` unless the blocked set names them, which is the fail-safe
// direction for a LABEL: a row wrongly marked blocked is a visible puzzle, one
// wrongly marked on is a lie about what a client can reach.
func (l *ToolList) mark(cat offlineCatalog) {
	out := make(map[string]bool, len(cat.blocked))
	for _, t := range cat.blocked {
		out[t.Exposed] = true
	}
	for i := range l.Rows {
		if out[l.Rows[i].Name] {
			l.Rows[i].State = toolStateBlocked
			l.Rows[i].BlockedBy = cat.blockedBy[l.Rows[i].Name]
			continue
		}
		l.Rows[i].State = toolStateOn
	}
}

// The layers an offline catalog can be computed under, and therefore the
// answers to "which one took this tool away". They are strings on the wire
// because the set grows downward: a client layer would join it, and a numeric
// depth would silently renumber every consumer when it did.
const (
	blockedByGlobal         = "global"
	blockedByProfileServers = "profile:servers"
	blockedByProfileTools   = "profile:tools"
)

// offlineCatalog aggregates the cached tools of the ENABLED servers (only
// serverArg when given) under the same exposed-name rules the gateway uses,
// and splits them by the narrowing layers it was asked for.
//
// The split is done by MERGING the layers, not by filtering the slice: a rule
// read straight off servers.json here would be a second implementation of
// what pipeline.ScopeAllows decides for every live call, and the two would
// eventually disagree about the case nobody tested. Only the CLIENT-specific
// layers are left out — there is no session to resolve them against — which
// is what makes the global answer "what the machine offers", the common
// prefix of what every client sees, and the profile answer the same thing one
// layer down.
//
// The router is built from the FULL cached set before the split, because
// exposed names come from collision handling across the whole catalog: a
// router built from the surviving tools alone could hand out a different
// name than the gateway does, which is the one thing a debugging view must
// not do.
type offlineCatalog struct {
	visible []discovery.Tool
	blocked []discovery.Tool
	// blockedBy names the layer each blocked tool was taken by, keyed by
	// exposed name. The VERDICT always comes from the merge; only the label
	// is read off the rules, and only for tools the merge already dropped.
	blockedBy map[string]string
	pending   []ToolRow // named by a rule, absent from every cached catalog
}

// offlineCatalogOf computes the catalog under the global layer, and under the
// named profile too when profileName is not empty. The profile is resolved
// through scope.PinnedProfileLayer, so a name that does not exist fail-closes
// to a block-all layer here exactly as it does for a session.
func offlineCatalogOf(
	snap *registry.Snapshot,
	cached map[string][]mcp.ToolDef,
	serverArg string,
	profileName string,
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
	catalog := router.NewCatalog(raw)

	all := discovery.Visible(rt, nil)
	out := offlineCatalog{visible: all, blockedBy: map[string]string{}}

	var layers []scope.ScopeLayer
	if layer, ok := scope.ServerToolsLayer(snap); ok {
		layers = append(layers, layer)
	}
	// Each layer is applied on top of the ones above it and the difference
	// attributed to it, so the label is derived from the same merge as the
	// verdict rather than from a second reading of the rules.
	survivors, err := visibleUnder(rt, catalog, layers)
	if err != nil {
		return offlineCatalog{}, fmt.Errorf("resolve the global allow list: %w", err)
	}
	out.visible = survivors
	out.attribute(all, survivors, func(discovery.Tool) string { return blockedByGlobal })

	var profile registry.Profile
	if profileName != "" {
		layer, ok := scope.PinnedProfileLayer(snap, profileName)
		layers = append(layers, layer)
		if ok {
			profile = snap.Profiles.V.Profiles[profileName].V
		}
		afterProfile, perr := visibleUnder(rt, catalog, layers)
		if perr != nil {
			return offlineCatalog{}, fmt.Errorf("resolve profile %q: %w", profileName, perr)
		}
		out.visible = afterProfile
		// A profile narrows on two axes and they need different repairs:
		// `profile server add` puts the server back, `profile tool allow`
		// widens the selector. Which of the two applies is read off the
		// profile, but only for tools the merge has already dropped.
		out.attribute(survivors, afterProfile, func(t discovery.Tool) string {
			if !profileIncludesServer(profile, t.ServerID) {
				return blockedByProfileServers
			}
			return blockedByProfileTools
		})
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
		if profileName == "" {
			continue
		}
		for _, name := range unknownRuleNames(profile.Tools[id].V.Allow, selected[id]) {
			out.pending = append(out.pending, ToolRow{
				Server: id, RawName: name, State: toolStatePending,
				BlockedBy:   blockedByProfileTools,
				Description: "named by the profile's allow list; no cached catalog has it",
			})
		}
	}
	return out, nil
}

// visibleUnder resolves what survives a set of layers. No layers at all means
// no narrowing, which is a merge that would have nothing to do — and a nil
// scope is what discovery.Visible already reads as "everything".
func visibleUnder(rt *router.Router, catalog router.Catalog, layers []scope.ScopeLayer) ([]discovery.Tool, error) {
	if len(layers) == 0 {
		return discovery.Visible(rt, nil), nil
	}
	eff, err := scope.Merge(layers, catalog)
	if err != nil {
		return nil, err
	}
	return discovery.Visible(rt, eff), nil
}

// attribute records everything present in before and absent from after as
// blocked, labelled by the layer that closed between the two.
func (c *offlineCatalog) attribute(before, after []discovery.Tool, by func(discovery.Tool) string) {
	live := make(map[string]bool, len(after))
	for _, t := range after {
		live[t.Exposed] = true
	}
	for _, t := range before {
		if live[t.Exposed] {
			continue
		}
		c.blocked = append(c.blocked, t)
		c.blockedBy[t.Exposed] = by(t)
	}
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
