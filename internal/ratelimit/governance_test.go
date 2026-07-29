package ratelimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/registry"
)

func govRules(rules ...registry.RateLimitRule) registry.GovernanceDoc {
	docs := make([]registry.Doc[registry.RateLimitRule], 0, len(rules))
	for _, r := range rules {
		docs = append(docs, registry.Doc[registry.RateLimitRule]{V: r})
	}
	return registry.GovernanceDoc{RateLimits: docs}
}

func TestConfigFromGovernance(t *testing.T) {
	cfg, err := ConfigFromGovernance(govRules(
		registry.RateLimitRule{Server: "gh", Limit: 30, Window: "1m"},
		registry.RateLimitRule{Client: "claude-code", Tool: "create_issue", Limit: 2, Window: "1h", Scope: "rule"},
	))
	if err != nil {
		t.Fatalf("ConfigFromGovernance: %v", err)
	}
	if len(cfg.Rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(cfg.Rules))
	}
	if cfg.Rules[0].ID() != "*/gh/*" || time.Duration(cfg.Rules[0].Window) != time.Minute {
		t.Errorf("rule 0 = %+v", cfg.Rules[0])
	}
	if cfg.Rules[1].Scope != ScopePerRule || time.Duration(cfg.Rules[1].Window) != time.Hour {
		t.Errorf("rule 1 = %+v", cfg.Rules[1])
	}
}

// An absent rule set is the norm, and it must translate into the disabled
// limiter — not into an error and not into an empty-but-enabled one.
func TestConfigFromGovernanceAbsentMeansDisabled(t *testing.T) {
	cfg, err := ConfigFromGovernance(registry.GovernanceDoc{})
	if err != nil {
		t.Fatalf("an absent rate limit block must not be an error: %v", err)
	}
	lim, err := New(Options{Config: cfg, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if lim.Enabled() {
		t.Fatal("an absent rate limit block must leave the limiter disabled")
	}
}

// Configuration is the one thing this package never fails open on: a rule it
// cannot interpret must reject the WHOLE set. Dropping the bad rule would
// present as "the quota is not working" with no evidence anywhere.
func TestConfigFromGovernanceRefusesBadRules(t *testing.T) {
	cases := []struct {
		name string
		gov  registry.GovernanceDoc
		want string
	}{
		{
			"bare number window",
			govRules(registry.RateLimitRule{Server: "gh", Limit: 5, Window: "60"}),
			"duration string",
		},
		{
			"empty window",
			govRules(registry.RateLimitRule{Server: "gh", Limit: 5}),
			"duration string",
		},
		{
			"unknown scope",
			govRules(registry.RateLimitRule{Server: "gh", Limit: 5, Window: "1m", Scope: "server"}),
			"unknown scope",
		},
		{
			"zero limit",
			govRules(registry.RateLimitRule{Server: "gh", Limit: 0, Window: "1m"}),
			"limit must be >= 1",
		},
		{
			"duplicate pattern",
			govRules(
				registry.RateLimitRule{Server: "gh", Limit: 5, Window: "1m"},
				registry.RateLimitRule{Server: "gh", Limit: 9, Window: "1h"},
			),
			"duplicate rule pattern",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ConfigFromGovernance(tc.gov)
			if err == nil {
				t.Fatalf("expected a refusal, got %d rules", len(cfg.Rules))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if len(cfg.Rules) != 0 {
				t.Fatal("a rejected rule set must not yield a partial config")
			}
		})
	}
}

// A quota this process cannot count is refused at assembly time rather than
// discovered as a permanently degraded (uncounted) call path. The gateway
// turns this into a startup failure.
func TestUnusableStateDirRefusesToBuild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny anything")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// With rules: refused.
	_, err := New(Options{
		Config:   Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}},
		StateDir: dir,
	})
	if err == nil {
		t.Fatal("an unusable counter file with rules configured must refuse to build")
	}
	if !strings.Contains(err.Error(), "counter file is unusable") {
		t.Fatalf("error %q must say the counter file is unusable", err)
	}

	// Without rules: the same directory is never touched, so nothing fails.
	// Zero configured quotas must cost exactly nothing.
	if _, err := New(Options{StateDir: dir}); err != nil {
		t.Fatalf("a limiter with no rules must not touch the state dir: %v", err)
	}
}
