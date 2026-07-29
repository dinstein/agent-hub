package ratelimit

import (
	"fmt"
	"time"

	"github.com/dinstein/agent-hub/internal/registry"
)

// This file holds the ONE translation from the on-disk governance document
// (governance.json `rateLimits`) into the rule set this package enforces.
//
// It lives here rather than in internal/registry for two reasons. The
// registry is the storage layer and stores configuration verbatim — giving
// it enforcement semantics (duration parsing, scope vocabulary, duplicate
// detection) would put the same rules in two packages. And the edge could
// not point the other way anyway: this package wraps internal/pipeline,
// which already depends on internal/registry, so a registry that imported
// ratelimit would close an import cycle.

// ConfigFromGovernance builds the rule set from a governance document. An
// absent rule set yields an empty Config, which disables enforcement
// entirely (Limiter.Enabled reports false and Guard leaves calls untouched).
//
// Failure direction: FAIL CLOSED ON CONFIGURATION. Every rule must parse and
// validate; one bad rule rejects the WHOLE set rather than being skipped.
// A silently dropped rule presents as "the quota is not working" with no
// evidence anywhere — the one thing this package never fails open on (see
// the package doc). The caller turns this error into a refusal to start.
func ConfigFromGovernance(g registry.GovernanceDoc) (Config, error) {
	if len(g.RateLimits) == 0 {
		return Config{}, nil
	}
	cfg := Config{Rules: make([]Rule, 0, len(g.RateLimits))}
	for i, doc := range g.RateLimits {
		raw := doc.V
		rule := Rule{
			Client: raw.Client,
			Server: raw.Server,
			Tool:   raw.Tool,
			Limit:  raw.Limit,
			Scope:  raw.Scope,
		}
		window, err := time.ParseDuration(raw.Window)
		if err != nil {
			// The window is a duration STRING on disk, so a bare 60 lands
			// here rather than being read as 60 of some unit nobody agreed
			// on (config.go, type Duration).
			return Config{}, fmt.Errorf(
				"ratelimit: governance rule %d (%s): window must be a duration string like \"1m\", got %q",
				i, rule.ID(), raw.Window)
		}
		rule.Window = Duration(window)
		cfg.Rules = append(cfg.Rules, rule)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
