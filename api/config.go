package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
)

// Governance key kinds, as reported by GovernanceKey.Kind.
const (
	// GovernanceKindBool is a boolean switch ("true"/"false").
	GovernanceKindBool = "bool"
	// GovernanceKindEnum is a fixed vocabulary (e.g. the discovery mode).
	GovernanceKindEnum = "enum"
	// GovernanceKindBytes is a byte budget, optionally suffixed "!" or
	// " forced" to make it merge tighten-only.
	GovernanceKindBytes = "bytes"
)

// ResultBudgetPrefix introduces the dynamic key family
// `resultBudget.<serverID|*>`, whose value is a byte budget.
const ResultBudgetPrefix = "resultBudget."

// safetyKeys are the governance switches whose "off" position WEAKENS
// enforcement. They merge tighten-only downward (boolean OR), so this
// surface is the only place they can ever be relaxed.
//
// The list is restated client-side on purpose: the wire carries no "this is
// a safety gate" flag, and a frontend must still be able to mark "you are
// about to turn one off". Deriving the warning from a field the server may
// omit would make the loud path the one that can silently go quiet.
var safetyKeys = []string{"denyDestructive", "blockOnInjection", "humanApproval"}

// IsSafetyKey reports whether relaxing key weakens a safety gate, so a
// frontend can demand a deliberate confirmation for it. Both the canonical
// camelCase name and the snake_case alias are recognised.
func IsSafetyKey(key string) bool {
	if slices.Contains(safetyKeys, key) {
		return true
	}
	// Accept the snake_case aliases the daemon also takes.
	switch key {
	case "deny_destructive", "block_on_injection", "human_approval":
		return true
	}
	return false
}

// GovernanceKey describes one settable governance field: the key table is
// frozen server-side and get/set/list all read the SAME table, because a key
// whose listing and whose setter disagree is a switch nobody can trust.
type GovernanceKey struct {
	// Key is the canonical name.
	Key string `json:"key"`
	// Kind is one of the GovernanceKind* constants.
	Kind string `json:"kind"`
	// Doc is the one-line human explanation.
	Doc string `json:"doc,omitempty"`
}

// Safety reports whether relaxing this key weakens a safety gate. It is
// derived client-side (IsSafetyKey) rather than read off the wire, so the
// "you are about to turn a gate off" confirmation still fires against a
// daemon that does not label its keys — the loud path must not be the one
// that can silently go quiet.
func (k GovernanceKey) Safety() bool { return IsSafetyKey(k.Key) }

// GovernanceValue is one key with its current value.
//
// Value is a STRING for every kind — it is the same rendering the CLI shows
// ("true", "grouped", "65536", "65536 (forced)"), so the two surfaces cannot
// disagree about what a switch currently says. "" means unset.
type GovernanceValue struct {
	GovernanceKey
	Value string `json:"value"`
}

// GovernanceList is the answer to Config.Keys: every key with its current
// value, plus the generation the following write sends back as its
// expectedGeneration.
type GovernanceList struct {
	Generation uint64            `json:"generation"`
	Entries    []GovernanceValue `json:"entries"`
}

// ConfigWrite is what a governance write returns. Previous is what the key
// said before, so a frontend can render the transition (and an operator can
// see which direction a safety gate moved).
type ConfigWrite struct {
	WriteResult
	Key      string `json:"key"`
	Value    string `json:"value"`
	Previous string `json:"previous,omitempty"`
}

// configSetBody is the PUT /v1/config/{key} request body.
type configSetBody struct {
	Value string `json:"value"`
}

// ConfigService reads and writes the GLOBAL governance layer.
//
// Every switch here merges tighten-only downward, so a lower layer can never
// undo one — which is exactly why this is an operator surface and not an
// agent-reachable one.
type ConfigService struct{ c *Client }

// Keys returns every governance key with its current value: the static table
// first, then the `resultBudget.<server>` entries actually present.
func (s *ConfigService) Keys(ctx context.Context) (GovernanceList, error) {
	var out GovernanceList
	err := s.c.do(ctx, http.MethodGet, "/config", nil, nil, &out)
	return out, err
}

// Get reads one key, resolving the snake_case aliases the daemon also
// accepts. found=false means "no such key", which is deliberately NOT the
// same answer as an empty Value ("the key exists and is unset"): a typo must
// never read as "unset" and be mistaken for a gate that is simply off.
//
// There is no per-key GET route — the key table is small and the listing is
// one round trip — so this selects from Keys rather than inventing an
// endpoint the daemon does not serve.
func (s *ConfigService) Get(ctx context.Context, key string) (GovernanceValue, bool, error) {
	list, err := s.Keys(ctx)
	if err != nil {
		return GovernanceValue{}, false, err
	}
	for _, e := range list.Entries {
		if e.Key == key || canonicalGovernanceKey(key) == e.Key {
			return e, true, nil
		}
	}
	return GovernanceValue{}, false, nil
}

// canonicalGovernanceKey maps the accepted snake_case aliases onto the
// canonical spelling. The daemon accepts both; a client that only compared
// literally would report "no such key" for a name the daemon would have set.
func canonicalGovernanceKey(key string) string {
	switch key {
	case "deny_destructive":
		return "denyDestructive"
	case "block_on_injection":
		return "blockOnInjection"
	case "human_approval":
		return "humanApproval"
	case "discovery_mode":
		return "discovery"
	default:
		return key
	}
}

// Set writes one governance key.
//
// Failure direction: an unparseable value is an error and leaves the switch
// untouched. A typo must never read as "false" and silently turn a
// governance gate off.
func (s *ConfigService) Set(
	ctx context.Context, key, value string, expectedGeneration uint64,
) (ConfigWrite, error) {
	var out ConfigWrite
	err := s.c.doWrite(ctx, http.MethodPut, "/config/"+url.PathEscape(key), nil,
		expectedGeneration, configSetBody{Value: value}, &out)
	return out, err
}
