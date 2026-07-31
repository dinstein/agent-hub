package confops

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// The governance surface edits governance.json — the GLOBAL layer of the
// scope chain. Key names are the registry field names, with snake_case
// aliases accepted because that is how the design examples spell them
// (`config set discovery_mode lazy`).
//
// Nothing here decides what a client may reach: that is settled by
// servers.json and profiles.json. What is left describes PRESENTATION — the
// default discovery mode and the result budgets — and the reason get/set/ls
// must all read the SAME table is unchanged: a key whose listing and whose
// setter disagree is a setting nobody can trust.

// GovernanceKey describes one settable governance field. Get and Set are the
// single place a key's semantics live.
type GovernanceKey struct {
	Name    string
	Aliases []string
	// Kind is "bool", "enum" or "bytes".
	Kind string
	Doc  string

	get func(g registry.GovernanceDoc) string
	set func(g *registry.GovernanceDoc, raw string) error
}

// Get renders the key's current value ("" when unset).
func (k GovernanceKey) Get(g registry.GovernanceDoc) string { return k.get(g) }

// governanceKeys is the frozen key table.
var governanceKeys = []GovernanceKey{
	{
		Name: "discovery", Aliases: []string{"discovery_mode"}, Kind: "enum",
		Doc: "default discovery mode: lazy, grouped or full",
		get: func(g registry.GovernanceDoc) string { return g.Discovery },
		set: func(g *registry.GovernanceDoc, raw string) error {
			if raw == "" || raw == "-" {
				g.Discovery = ""
				return nil
			}
			if err := ValidateDiscovery(raw); err != nil {
				return err
			}
			g.Discovery = raw
			return nil
		},
	},
}

// ResultBudgetPrefix introduces the dynamic key family
// `resultBudget.<serverID|*>`, whose value is a byte budget.
const ResultBudgetPrefix = "resultBudget."

// GovernanceKeys returns the frozen key table. The returned slice is a copy,
// so a front end can sort or filter it without mutating the source of truth.
func GovernanceKeys() []GovernanceKey {
	return append([]GovernanceKey(nil), governanceKeys...)
}

// LookupGovernanceKey resolves a canonical name or one of its aliases.
func LookupGovernanceKey(name string) (GovernanceKey, bool) {
	for _, k := range governanceKeys {
		if k.Name == name {
			return k, true
		}
		for _, alias := range k.Aliases {
			if alias == name {
				return k, true
			}
		}
	}
	return GovernanceKey{}, false
}

// GovernanceEntry is one key with its current value.
type GovernanceEntry struct {
	Key   string
	Value string
	Kind  string
	Doc   string
}

// budgetDoc is the doc string of every dynamic result-budget key.
const budgetDoc = "result payload byte budget"

// GetGovernance reads one key (static or `resultBudget.<id>`) out of a
// governance document.
func GetGovernance(g registry.GovernanceDoc, name string) (GovernanceEntry, error) {
	if budgetKey, ok := strings.CutPrefix(name, ResultBudgetPrefix); ok {
		return GovernanceEntry{Key: name, Kind: "bytes", Value: budgetValue(g, budgetKey), Doc: budgetDoc}, nil
	}
	k, ok := LookupGovernanceKey(name)
	if !ok {
		return GovernanceEntry{}, UnknownGovernanceKey(name)
	}
	return GovernanceEntry{Key: k.Name, Kind: k.Kind, Value: k.get(g), Doc: k.Doc}, nil
}

// ListGovernance renders every static key followed by the result-budget keys
// actually present, in ascending server order.
func ListGovernance(g registry.GovernanceDoc) []GovernanceEntry {
	out := make([]GovernanceEntry, 0, len(governanceKeys)+len(g.ResultBudget))
	for _, k := range governanceKeys {
		out = append(out, GovernanceEntry{Key: k.Name, Kind: k.Kind, Value: k.get(g), Doc: k.Doc})
	}
	for _, id := range sortedKeys(g.ResultBudget) {
		out = append(out, GovernanceEntry{
			Key: ResultBudgetPrefix + id, Kind: "bytes", Value: budgetValue(g, id), Doc: budgetDoc,
		})
	}
	return out
}

// GovernanceResult is what SetGovernance returns.
type GovernanceResult struct {
	Result
	Key      string
	Value    string
	Previous string
}

// SetGovernance writes one governance key.
//
// Failure direction: an unparseable value is an error and leaves the switch
// untouched. A typo must never read as "false" and silently turn a
// governance gate off.
func SetGovernance(
	ctx context.Context, st *registry.Store, name, raw string, pre Precondition,
) (GovernanceResult, error) {
	out := GovernanceResult{}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		g := tx.Governance.V
		if budgetKey, ok := strings.CutPrefix(name, ResultBudgetPrefix); ok {
			out.Key, out.Previous = name, budgetValue(g, budgetKey)
			if err := setBudget(&g, budgetKey, raw); err != nil {
				return err
			}
			out.Value = budgetValue(g, budgetKey)
		} else {
			k, ok := LookupGovernanceKey(name)
			if !ok {
				return UnknownGovernanceKey(name)
			}
			out.Key, out.Previous = k.Name, k.get(g)
			if err := k.set(&g, raw); err != nil {
				return err
			}
			out.Value = k.get(g)
		}
		tx.Governance.V = g
		return nil
	})
	out.Result = res
	if err != nil {
		return out, err
	}
	// Changed is the semantic answer (did the VALUE move), which is what a
	// front end reports. It matches the generation-derived Result.Changed
	// except for a rewrite that normalizes spelling without moving the value.
	out.Changed = out.Value != out.Previous
	return out, nil
}

// UnknownGovernanceKey is the shared refusal naming every valid key, so the
// operator never has to guess the spelling.
func UnknownGovernanceKey(name string) *Error {
	names := make([]string, 0, len(governanceKeys)+1)
	for _, k := range governanceKeys {
		names = append(names, k.Name)
	}
	names = append(names, ResultBudgetPrefix+"<server|*>")
	return &Error{
		Kind: KindUsage, Code: CodeConfigKeyUnknown,
		Message: fmt.Sprintf("unknown config key %q", name),
		Hint:    "known keys: " + strings.Join(names, ", "),
	}
}

// budgetValue renders one result-budget entry ("" when unset). A forced
// budget is marked because it merges tighten-only (min) rather than
// most-specific-wins, which changes what a lower layer can do with it.
func budgetValue(g registry.GovernanceDoc, id string) string {
	doc, ok := g.ResultBudget[id]
	if !ok {
		return ""
	}
	if doc.V.Forced {
		return strconv.Itoa(doc.V.Bytes) + " (forced)"
	}
	return strconv.Itoa(doc.V.Bytes)
}

// setBudget parses "<bytes>" or "<bytes> forced" (also "<bytes>!"), and
// clears the entry on "-" or an empty value.
func setBudget(g *registry.GovernanceDoc, id, raw string) error {
	if strings.TrimSpace(id) == "" {
		return usagef("%s<server|*> needs a server id after the dot", ResultBudgetPrefix)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "-" {
		delete(g.ResultBudget, id)
		return nil
	}
	forced := false
	if rest, ok := strings.CutSuffix(raw, "!"); ok {
		forced, raw = true, strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutSuffix(raw, " forced"); ok {
		forced, raw = true, strings.TrimSpace(rest)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return usagef("%s%s expects a non-negative byte count, got %q", ResultBudgetPrefix, id, raw)
	}
	if g.ResultBudget == nil {
		g.ResultBudget = map[string]registry.Doc[registry.Budget]{}
	}
	g.ResultBudget[id] = registry.Doc[registry.Budget]{V: registry.Budget{Bytes: n, Forced: forced}}
	return nil
}
