package ratelimit

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurationJSONRoundTrip(t *testing.T) {
	var r Rule
	if err := json.Unmarshal([]byte(`{"limit":3,"window":"90s"}`), &r); err != nil {
		t.Fatal(err)
	}
	if time.Duration(r.Window) != 90*time.Second {
		t.Fatalf("window = %v, want 90s", time.Duration(r.Window))
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"limit":3,"window":"1m30s"}`; got != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

// A bare number is ambiguous between seconds/millis/nanos, so it must be a
// loud error rather than a 1000x wrong quota.
func TestDurationRejectsNumber(t *testing.T) {
	var r Rule
	if err := json.Unmarshal([]byte(`{"limit":3,"window":60}`), &r); err == nil {
		t.Fatal("numeric window must be rejected")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"empty is valid", Config{}, false},
		{"ok", Config{Rules: []Rule{{Server: "gh", Limit: 5, Window: Duration(time.Minute)}}}, false},
		{"zero limit", Config{Rules: []Rule{{Limit: 0, Window: Duration(time.Minute)}}}, true},
		{"negative limit", Config{Rules: []Rule{{Limit: -1, Window: Duration(time.Minute)}}}, true},
		{"zero window", Config{Rules: []Rule{{Limit: 1}}}, true},
		{"sub-millisecond window", Config{Rules: []Rule{{Limit: 1, Window: Duration(500 * time.Microsecond)}}}, true},
		{"unknown scope", Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute), Scope: "global"}}}, true},
		{"per-rule scope", Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute), Scope: ScopePerRule}}}, false},
		{"duplicate pattern", Config{Rules: []Rule{
			{Server: "gh", Limit: 1, Window: Duration(time.Minute)},
			{Server: "gh", Limit: 2, Window: Duration(time.Hour)},
		}}, true},
		{"wildcard vs empty is the same pattern", Config{Rules: []Rule{
			{Client: "*", Limit: 1, Window: Duration(time.Minute)},
			{Client: "", Limit: 2, Window: Duration(time.Minute)},
		}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// Configuration errors are the ONE thing this package does not fail open on.
func TestNewRejectsInvalidConfig(t *testing.T) {
	_, err := New(Options{Config: Config{Rules: []Rule{{Limit: 0}}}, StateDir: t.TempDir()})
	if err == nil {
		t.Fatal("New must reject an invalid rule set instead of silently disabling quotas")
	}
}

func TestMatchReturnsEveryMatchingRule(t *testing.T) {
	cfg := Config{Rules: []Rule{
		{Limit: 100, Window: Duration(time.Minute)},                                 // */*/*
		{Server: "gh", Limit: 10, Window: Duration(time.Minute)},                    // */gh/*
		{Server: "gh", Tool: "create_issue", Limit: 2, Window: Duration(time.Hour)}, // */gh/create_issue
		{Server: "fs", Limit: 1, Window: Duration(time.Minute)},                     // */fs/*
	}}
	got := cfg.match(Key{Client: "claude-code", Server: "gh", Tool: "create_issue"})
	if len(got) != 3 {
		t.Fatalf("match returned %d rules, want 3 (AND semantics: every matching rule applies)", len(got))
	}
	want := []string{"*/*/*", "*/gh/*", "*/gh/create_issue"}
	for i, r := range got {
		if r.ID() != want[i] {
			t.Fatalf("rule %d = %s, want %s", i, r.ID(), want[i])
		}
	}
}

func TestKeyWithEmptyClientMatchesOnlyWildcards(t *testing.T) {
	cfg := Config{Rules: []Rule{{Client: "cursor", Limit: 1, Window: Duration(time.Minute)}}}
	if got := cfg.match(Key{Server: "gh", Tool: "t"}); len(got) != 0 {
		t.Fatalf("an unknown client must not match a client-specific rule, got %v", got)
	}
}

func TestEncodeKeyEscapesSeparators(t *testing.T) {
	a := encodeKey("*/*/*", Key{Client: "a|b", Server: "c", Tool: "d"})
	b := encodeKey("*/*/*", Key{Client: "a", Server: "b|c", Tool: "d"})
	if a == b {
		t.Fatalf("separator injection collapsed two distinct keys onto %q", a)
	}
	if got := encodeKey("r", Key{Client: "100%", Server: "s", Tool: "t"}); got != "r|100%25|s|t" {
		t.Fatalf("encodeKey = %q", got)
	}
}

// A per-rule rule shares ONE bucket across every key it matches; a per-key
// rule gives each triple its own.
func TestCounterKeyScope(t *testing.T) {
	shared := Rule{Server: "gh", Limit: 1, Window: Duration(time.Minute), Scope: ScopePerRule}
	perKey := Rule{Server: "gh", Limit: 1, Window: Duration(time.Minute)}
	a := Key{Client: "c", Server: "gh", Tool: "one"}
	b := Key{Client: "c", Server: "gh", Tool: "two"}

	if shared.counterKey(a) != shared.counterKey(b) {
		t.Fatal("ScopePerRule must collapse every matching key onto one bucket")
	}
	if perKey.counterKey(a) == perKey.counterKey(b) {
		t.Fatal("ScopePerKey must give each (client, server, tool) triple its own bucket")
	}
}
