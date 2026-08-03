package confops

import (
	"context"
	"fmt"
	"net"
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
// servers.json and profiles.json. What is left is everything global that is
// NOT a scope decision — presentation (the default discovery mode, the result
// budgets), the audit policy, and the daemon's own HTTP listener. The last of
// those arrived when the desktop application became the thing that starts a
// hub: an application does not type flags, so an opt-in that only existed as
// argv could no longer be given at all. Storing it does not lower any bar —
// a non-loopback bind still needs its explicit confirmation, and the
// credential-less endpoint still needs its own.
//
// The reason get/set/ls must all read the SAME table is unchanged: a key
// whose listing and whose setter disagree is a setting nobody can trust.

// GovernanceKey describes one settable governance field. Get and Set are the
// single place a key's semantics live.
type GovernanceKey struct {
	Name    string
	Aliases []string
	// Kind is "bool", "enum", "integer" or "bytes".
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
	{
		Name: "calls.enabled", Aliases: []string{"audit.enabled"}, Kind: "bool", Doc: "record every tools/call attempt; storage failure blocks execution",
		get: func(g registry.GovernanceDoc) string { return strconv.FormatBool(g.ResolvedCalls().Enabled) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := parseBool("calls.enabled", raw)
			if err != nil {
				return err
			}
			if v && g.ResolvedCalls().KeyID == "" {
				return usagef("the call ledger has no encryption key; use 'agenthub calls enable'")
			}
			callsForWrite(g).Enabled = v
			return nil
		},
	},
	{
		Name: "calls.durability", Aliases: []string{"audit.durability"}, Kind: "enum", Doc: "write acknowledgement: sync or write",
		get: func(g registry.GovernanceDoc) string { return g.ResolvedCalls().Durability },
		set: func(g *registry.GovernanceDoc, raw string) error {
			if raw != "sync" && raw != "write" {
				return usagef("calls.durability expects sync or write, got %q", raw)
			}
			callsForWrite(g).Durability = raw
			return nil
		},
	},
	{
		Name: "calls.results", Aliases: []string{"audit.results"}, Kind: "enum", Doc: "result capture: none, errors, truncated or full",
		get: func(g registry.GovernanceDoc) string { return g.ResolvedCalls().ResultMode },
		set: func(g *registry.GovernanceDoc, raw string) error {
			switch raw {
			case "none", "errors", "truncated", "full":
				callsForWrite(g).ResultMode = raw
				return nil
			default:
				return usagef("calls.results expects none, errors, truncated or full, got %q", raw)
			}
		},
	},
	{
		Name: "calls.resultBytes", Aliases: []string{"audit.resultBytes", "audit.result_bytes"}, Kind: "bytes",
		Doc: "maximum captured result bytes in truncated mode",
		get: func(g registry.GovernanceDoc) string { return strconv.Itoa(g.ResolvedCalls().ResultBytes) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := positiveInt("calls.resultBytes", raw, 16<<20)
			if err != nil {
				return err
			}
			callsForWrite(g).ResultBytes = v
			return nil
		},
	},
	{
		Name: "calls.retentionDays", Aliases: []string{"audit.retentionDays", "audit.retention_days"}, Kind: "integer",
		Doc: "days retained before a complete UTC partition may be pruned",
		get: func(g registry.GovernanceDoc) string { return strconv.Itoa(g.ResolvedCalls().RetentionDays) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := positiveInt("calls.retentionDays", raw, 3650)
			if err != nil {
				return err
			}
			callsForWrite(g).RetentionDays = v
			return nil
		},
	},
	{
		Name: "calls.maxBytes", Aliases: []string{"audit.maxBytes", "audit.max_bytes"}, Kind: "bytes",
		Doc: "hard total ledger size; new calls block instead of deleting unexpired records",
		get: func(g registry.GovernanceDoc) string { return strconv.FormatInt(g.ResolvedCalls().MaxBytes, 10) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := positiveInt64("audit.maxBytes", raw)
			if err != nil {
				return err
			}
			callsForWrite(g).MaxBytes = v
			return nil
		},
	},
	{
		Name: "calls.minFreeBytes", Aliases: []string{"audit.minFreeBytes", "audit.min_free_bytes"}, Kind: "bytes",
		Doc: "free-disk reserve; new calls block before crossing it",
		get: func(g registry.GovernanceDoc) string { return strconv.FormatInt(g.ResolvedCalls().MinFreeBytes, 10) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := positiveInt64("audit.minFreeBytes", raw)
			if err != nil {
				return err
			}
			callsForWrite(g).MinFreeBytes = v
			return nil
		},
	},
	{
		Name: "http.addr", Aliases: []string{"http_addr"}, Kind: "string",
		Doc: "serve MCP over Streamable HTTP on this host:port; unset means no listener at all",
		get: func(g registry.GovernanceDoc) string { return g.ResolvedHTTP().Addr },
		set: func(g *registry.GovernanceDoc, raw string) error {
			if raw == "" || raw == "-" {
				httpForWrite(g).Addr = ""
				return nil
			}
			if err := validateHostPort(raw); err != nil {
				return err
			}
			httpForWrite(g).Addr = raw
			return nil
		},
	},
	{
		Name: "http.allowRemote", Aliases: []string{"http_allow_remote"}, Kind: "bool",
		Doc: "confirm a non-loopback http.addr; without it a non-loopback address refuses to serve",
		get: func(g registry.GovernanceDoc) string { return strconv.FormatBool(g.ResolvedHTTP().AllowRemote) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := parseBool("http.allowRemote", raw)
			if err != nil {
				return err
			}
			httpForWrite(g).AllowRemote = v
			return nil
		},
	},
	{
		Name: "http.insecureLoopback", Aliases: []string{"http_insecure_loopback"}, Kind: "bool",
		Doc: "accept unauthenticated loopback callers on http.addr; never covers a non-loopback bind",
		get: func(g registry.GovernanceDoc) string {
			return strconv.FormatBool(g.ResolvedHTTP().InsecureLoopback)
		},
		set: func(g *registry.GovernanceDoc, raw string) error {
			v, err := parseBool("http.insecureLoopback", raw)
			if err != nil {
				return err
			}
			httpForWrite(g).InsecureLoopback = v
			return nil
		},
	},
	{
		Name: "events.enabled", Kind: "bool",
		Doc: "record server/gateway/daemon state changes (default on; a failed write is dropped, never a refused call)",
		get: func(g registry.GovernanceDoc) string { return strconv.FormatBool(g.EventsEnabled()) },
		set: func(g *registry.GovernanceDoc, raw string) error {
			// "-" clears the field back to the default rather than writing
			// the default's value, so `config ls` keeps showing "nobody set
			// this" instead of "someone chose on". For a tri-state the two
			// are different facts.
			if raw == "-" {
				g.Events = nil
				return nil
			}
			v, err := parseBool("events.enabled", raw)
			if err != nil {
				return err
			}
			g.Events = &v
			return nil
		},
	},
}

func httpForWrite(g *registry.GovernanceDoc) *registry.HTTPFace {
	if g.HTTP == nil {
		g.HTTP = &registry.Doc[registry.HTTPFace]{}
	}
	return &g.HTTP.V
}

// validateHostPort refuses anything that is not a bindable "host:port".
//
// It is a shape check, not a reachability one: the daemon will refuse a
// non-loopback address without its confirmation, and will fail to bind an
// address that is not this machine's. What this catches is the typo that
// would otherwise be discovered at the next start of the hub — by which time
// the person who wrote it is not watching.
func validateHostPort(raw string) error {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return usagef("http.addr expects host:port (e.g. localhost:7777), got %q", raw)
	}
	if port == "" {
		return usagef("http.addr needs a port, got %q", raw)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return usagef("http.addr port must be 1-65535, got %q", port)
	}
	// An empty host is legal and means every interface — which is exactly the
	// case that needs the confirmation, so it is accepted here and decided
	// there rather than being silently rewritten to loopback.
	_ = host
	return nil
}

// callsForWrite returns the policy to mutate, moving a pre-rename document
// into the current key on the way.
//
// The old key is dropped in the same write rather than left behind: two keys
// holding two policies is a file whose meaning depends on which one a reader
// looks at first, and the reader that looks at the stale one enforces a
// setting nobody chose.
func callsForWrite(g *registry.GovernanceDoc) *registry.CallsPolicy {
	if g.Calls == nil {
		g.Calls = &registry.Doc[registry.CallsPolicy]{}
		if g.Audit != nil {
			*g.Calls = *g.Audit
		}
	}
	g.Audit = nil
	return &g.Calls.V
}

func parseBool(name, raw string) (bool, error) {
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, usagef("%s expects true or false, got %q", name, raw)
	}
}

func positiveInt(name, raw string, max int) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > max {
		return 0, usagef("%s expects an integer from 1 through %d, got %q", name, max, raw)
	}
	return v, nil
}

func positiveInt64(name, raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, usagef("%s expects a positive byte count, got %q", name, raw)
	}
	return v, nil
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

// CallsPolicyResult is the semantic result of enabling or disabling the
// ledger. Enabling materializes every current default so an upgrade cannot
// silently change the retention or capture contract of an existing install.
type CallsPolicyResult struct {
	Result
	Previous registry.ResolvedCallsPolicy
	Policy   registry.ResolvedCallsPolicy
}

// SetCallsEnabled changes the ledger's master switch. keyID is required when
// enabling and is public key metadata, not the encryption key itself.
func SetCallsEnabled(
	ctx context.Context, st *registry.Store, enabled bool, keyID string, pre Precondition,
) (CallsPolicyResult, error) {
	out := CallsPolicyResult{}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		g := tx.Governance.V
		out.Previous = g.ResolvedCalls()
		if !enabled && g.CallsDoc() == nil {
			out.Policy = out.Previous
			return nil
		}
		if enabled && strings.TrimSpace(keyID) == "" {
			return usagef("enabling the call ledger requires a key id")
		}
		p := callsForWrite(&g)
		p.Enabled = enabled
		if enabled {
			resolved := g.ResolvedCalls()
			p.Durability = resolved.Durability
			p.ResultMode = resolved.ResultMode
			p.ResultBytes = resolved.ResultBytes
			p.RetentionDays = resolved.RetentionDays
			p.MaxBytes = resolved.MaxBytes
			p.MinFreeBytes = resolved.MinFreeBytes
			p.KeyID = keyID
		}
		tx.Governance.V = g
		out.Policy = g.ResolvedCalls()
		return nil
	})
	out.Result = res
	if err != nil {
		return out, err
	}
	out.Changed = out.Previous != out.Policy
	return out, nil
}

// SetAuditKeyID advances the ledger to an already-persisted immutable key.
// It never changes the enabled bit, so rotation is safe while capture is
// paused as well as while it is live.
func SetAuditKeyID(
	ctx context.Context, st *registry.Store, keyID string, pre Precondition,
) (CallsPolicyResult, error) {
	out := CallsPolicyResult{}
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		g := tx.Governance.V
		out.Previous = g.ResolvedCalls()
		if doc := g.CallsDoc(); doc == nil || strings.TrimSpace(doc.V.KeyID) == "" {
			return usagef("the ledger has no current key; run agenthub calls enable first")
		}
		if strings.TrimSpace(keyID) == "" {
			return usagef("rotating the ledger key requires a key id")
		}
		p := callsForWrite(&g)
		p.KeyID = keyID
		tx.Governance.V = g
		out.Policy = g.ResolvedCalls()
		return nil
	})
	out.Result = res
	if err != nil {
		return out, err
	}
	out.Changed = out.Previous != out.Policy
	return out, nil
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
