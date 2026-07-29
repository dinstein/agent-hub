package ratelimit

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Wildcard matches any value in a rule dimension. An absent (empty) field
// means the same thing; Normalize rewrites "" to Wildcard so a rule's
// identity string is unambiguous.
const Wildcard = "*"

// Key is the quota dimension: (client, server, tool).
// Server/Tool are the ROUTED values (router.RouteOf provenance), never the
// exposed name — a rename must not change which quota a call spends from.
// Client is the caller identity the assembling gateway knows (clients.json
// id for a stdio gateway, the session's client id on the HTTP face); "" is
// legal and matches only Wildcard rules.
type Key struct {
	Client string
	Server string
	Tool   string
}

// String renders the key for logs and audit records. It carries no
// arguments and no payload — only identifiers.
func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s", orWildcard(k.Client), orWildcard(k.Server), orWildcard(k.Tool))
}

func orWildcard(s string) string {
	if s == "" {
		return Wildcard
	}
	return s
}

// Duration is a time.Duration that marshals as a Go duration STRING
// ("30s", "1m", "1h"). Numbers are rejected on purpose: a bare 60 in a
// config file is ambiguous between seconds, millis and nanos, and the
// ambiguity would be discovered as a 1000x wrong quota in production.
type Duration time.Duration

// MarshalJSON writes the canonical duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts only a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("ratelimit: window must be a duration string like \"1m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("ratelimit: parse window %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Counter scopes: what a rule's Limit is counted PER.
//
// The distinction is not cosmetic. "30 calls a minute to the github server"
// and "30 calls a minute to each github tool" are different quotas, and a
// rule language that can only express one of them silently gives operators
// the other.
const (
	// ScopePerKey (the default) gives every distinct matching
	// (client, server, tool) triple its own bucket — the dimension
	// names.
	ScopePerKey = "key"
	// ScopePerRule gives the rule ONE shared bucket across every key it
	// matches: the way to write a server-wide or client-wide cap.
	ScopePerRule = "rule"
)

// Rule is one quota: at most Limit calls per Window, counted per Scope,
// for the keys matching (Client, Server, Tool).
//
// The bucket is a token bucket with capacity Limit refilling at Limit per
// Window, so a burst of Limit is allowed and then the rate is enforced
// smoothly — a fixed window would let 2*Limit through across a window
// boundary.
type Rule struct {
	Client string   `json:"client,omitempty"`
	Server string   `json:"server,omitempty"`
	Tool   string   `json:"tool,omitempty"`
	Limit  int      `json:"limit"`
	Window Duration `json:"window"`
	// Scope is ScopePerKey (default, "" means the default) or ScopePerRule.
	Scope string `json:"scope,omitempty"`
}

// counterKey is the on-disk counter name for this rule and one call key.
// Under ScopePerRule every matching call shares one bucket; under
// ScopePerKey each triple gets its own.
func (r Rule) counterKey(k Key) string {
	if r.Scope == ScopePerRule {
		return encodeKey(r.ID(), Key{})
	}
	return encodeKey(r.ID(), k)
}

// ID is the rule's stable identity: its pattern triple. It keys the
// on-disk counters and names the rule in errors and audit records, so two
// rules may not share a pattern (Validate rejects that).
func (r Rule) ID() string {
	return fmt.Sprintf("%s/%s/%s", orWildcard(r.Client), orWildcard(r.Server), orWildcard(r.Tool))
}

// matches reports whether the rule applies to key. Each dimension matches
// exactly or by Wildcard; there is no prefix or glob syntax, because a
// half-understood pattern language is how a quota ends up applying to
// nothing.
func (r Rule) matches(k Key) bool {
	return dimMatches(r.Client, k.Client) && dimMatches(r.Server, k.Server) && dimMatches(r.Tool, k.Tool)
}

func dimMatches(pattern, value string) bool {
	if pattern == "" || pattern == Wildcard {
		return true
	}
	return pattern == value
}

// Config is the rule set, as it appears under the governance key
// `rateLimits`. An empty/absent Config means no quotas at all — the
// package is opt-in, exactly like every other M2 switch.
type Config struct {
	Rules []Rule `json:"rules,omitempty"`
}

// Validate rejects a rule set that cannot mean what it says. Config errors
// are the one thing this package does NOT fail open on: silently dropping
// an unparsable rule would present as "the quota is not working" with no
// evidence anywhere.
func (c Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Rules))
	for i, r := range c.Rules {
		switch {
		case r.Limit < 1:
			return fmt.Errorf("ratelimit: rule %d (%s): limit must be >= 1, got %d", i, r.ID(), r.Limit)
		case time.Duration(r.Window) <= 0:
			return fmt.Errorf("ratelimit: rule %d (%s): window must be > 0", i, r.ID())
		case time.Duration(r.Window) < time.Millisecond:
			// Sub-millisecond windows cannot be represented by the
			// millisecond-resolution counter clock; refusing beats
			// pretending.
			return fmt.Errorf("ratelimit: rule %d (%s): window must be >= 1ms", i, r.ID())
		}
		if r.Scope != "" && r.Scope != ScopePerKey && r.Scope != ScopePerRule {
			return fmt.Errorf("ratelimit: rule %d (%s): unknown scope %q (want %q or %q)",
				i, r.ID(), r.Scope, ScopePerKey, ScopePerRule)
		}
		if strings.ContainsAny(r.Client+r.Server+r.Tool, "\x00\n") {
			return fmt.Errorf("ratelimit: rule %d (%s): pattern must not contain NUL or newline", i, r.ID())
		}
		id := r.ID()
		if _, dup := seen[id]; dup {
			return fmt.Errorf("ratelimit: duplicate rule pattern %q (rule ids key the on-disk counters)", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// match returns every rule applying to key, in configuration order.
//
// ALL matching rules are enforced (logical AND), never "most specific
// wins". A quota set merges the same direction as every other governance
// field in this codebase: monotonically tighter. A narrow rule can only
// ever restrict further, never unlock what a broad rule forbids.
func (c Config) match(k Key) []Rule {
	var out []Rule
	for _, r := range c.Rules {
		if r.matches(k) {
			out = append(out, r)
		}
	}
	return out
}
