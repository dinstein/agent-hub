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
)

// `tool ls` is the OFFLINE view of the aggregated catalog: it reads the
// registry (which servers are configured and enabled) plus the gateway's
// persisted tool cache, and never connects to anything. That is the same
// pair of inputs a cold gateway answers tools/list from (docs/flows.md), so
// the CLI shows what a client would see rather than a second opinion.
//
// --search reuses internal/discovery's ranker — the SAME scoring, tie-break
// and truncation the lazy-mode search_tools meta-tool uses. Two rankers
// would eventually disagree, and the one the operator debugs with must be
// the one the agent gets.

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
	Rank        int    `json:"rank,omitempty"`
	Score       int    `json:"score,omitempty"`
}

// ToolList is the `tool ls` result. Ranked selects the search rendering;
// the JSON shape is a plain array either way.
type ToolList struct {
	Rows   []ToolRow
	Ranked bool
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
		if l.Ranked {
			_, err := fmt.Fprintln(w, "no tool matches this query")
			return err
		}
		_, err := fmt.Fprintln(w, "no tools cached yet (connect a client once so the gateway can populate the cache)")
		return err
	}
	// Writes to a tabwriter only fail at Flush, which is where the error is
	// surfaced.
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if l.Ranked {
		_, _ = fmt.Fprintln(tw, "RANK\tSCORE\tNAME\tSERVER\tTOOL\tDESCRIPTION")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tSERVER\tTOOL\tDESCRIPTION")
	}
	for _, r := range l.Rows {
		desc := oneLine(r.Description, descriptionColumnBytes)
		if l.Ranked {
			_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", r.Rank, r.Score, r.Name, r.Server, r.RawName, desc)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Server, r.RawName, desc)
	}
	return tw.Flush()
}

func (a *App) newToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tool",
		Aliases: []string{"tools"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Inspect the aggregated tool catalog",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newToolLsCmd())
	cmd.AddCommand(a.newToolAllowCmd())
	return cmd
}

func (a *App) newToolLsCmd() *cobra.Command {
	var search string
	cmd := &cobra.Command{
		Use:   "ls [<server>]",
		Short: "List cached tools, optionally ranked by a search query",
		Long: "List the tools of the configured servers from the gateway's tool cache.\n\n" +
			"With --search the results are ranked by the same lexical ranker the\n" +
			fmt.Sprintf("lazy-mode search_tools meta-tool uses, best match first, capped at %d results.",
				discovery.MaxSearchLimit),
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
			surface, err := offlineSurface(snap.Servers.V.Servers, cached, serverArg, snap.Generation)
			if err != nil {
				return err
			}

			list, err := renderTools(surface, search)
			if err != nil {
				return err
			}
			return a.printer().Emit(list, warnings...)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "rank the tools against a keyword query")
	return cmd
}

// offlineSurface aggregates the cached tools of the ENABLED servers (only
// serverArg when given) under the same exposed-name rules the gateway uses,
// then wraps them in a full-mode discovery surface.
//
// Scope is deliberately nil: the CLI has no session, and a scope resolved
// against no client would be a different answer than any client actually
// gets. The surface therefore shows the whole catalog — the operator's view,
// stated as such by the command, not an agent's view.
func offlineSurface(
	servers map[string]registry.Doc[registry.ServerEntry],
	cached map[string][]mcp.ToolDef,
	serverArg string,
	generation uint64,
) (*discovery.Surface, error) {
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
		return nil, fmt.Errorf("aggregate cached tools: %w", err)
	}
	return discovery.New(discovery.Options{
		Mode:       discovery.ModeFull,
		Tools:      discovery.Visible(rt, nil),
		Generation: generation,
	}), nil
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
