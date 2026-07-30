package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigGetSetLs(t *testing.T) {
	dir := setDataDir(t)

	// An unset key reads as its zero value, not as an error.
	var entry ConfigEntry
	decodeInto(t, mustRun(t, "", "config", "get", "discovery", "--json"), &entry)
	if entry.Value != "" {
		t.Errorf("unset key = %q, want empty", entry.Value)
	}

	var set ConfigSetResult
	decodeInto(t, mustRun(t, "", "config", "set", "discovery_mode", "lazy", "--json"), &set)
	if !set.Changed || set.Value != "lazy" || set.Key != "discovery" {
		t.Errorf("set via snake_case alias = %+v, want the canonical key and a change", set)
	}
	// Idempotent re-set reports no change.
	var again ConfigSetResult
	decodeInto(t, mustRun(t, "", "config", "set", "discovery", "lazy", "--json"), &again)
	if again.Changed {
		t.Errorf("re-setting the same value reported a change: %+v", again)
	}

	// The value actually reached governance.json.
	raw, err := os.ReadFile(filepath.Join(dir, "registry", "governance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gov map[string]any
	if err := json.Unmarshal(raw, &gov); err != nil {
		t.Fatal(err)
	}
	if gov["discovery"] != "lazy" {
		t.Errorf("governance.json = %s", raw)
	}

	// Enum validation and boolean validation both refuse rather than
	// silently reading as "off".
	if code, _, _ := runCLI(t, "", "config", "set", "discovery", "bogus"); code != ExitUsage {
		t.Errorf("bad enum exit = %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runCLI(t, "", "config", "set", "discovery", "maybe"); code != ExitUsage {
		t.Errorf("bad bool exit = %d, want %d", code, ExitUsage)
	}
	if code, _, stderr := runCLI(t, "", "config", "get", "nope"); code != ExitUsage {
		t.Errorf("unknown key exit = %d, want %d (%s)", code, ExitUsage, stderr)
	}

	mustRun(t, "", "config", "set", "discovery", "grouped")
	var list ConfigList
	decodeInto(t, mustRun(t, "", "config", "ls", "--json"), &list)
	seen := map[string]string{}
	for _, e := range list.Entries {
		seen[e.Key] = e.Value
	}
	for _, want := range []string{"discovery"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("config ls is missing %q: %+v", want, list.Entries)
		}
	}
	if seen["discovery"] != "grouped" {
		t.Errorf("config ls values = %v", seen)
	}
}

func TestConfigResultBudget(t *testing.T) {
	setDataDir(t)
	var set ConfigSetResult
	decodeInto(t, mustRun(t, "", "config", "set", "resultBudget.*", "65536", "--json"), &set)
	if set.Value != "65536" {
		t.Errorf("budget = %+v", set)
	}
	// The "!" suffix marks a forced budget, which merges by MIN instead of
	// most-specific-wins — a different rule, so it must be visible.
	var forced ConfigSetResult
	decodeInto(t, mustRun(t, "", "config", "set", "resultBudget.github", "1024!", "--json"), &forced)
	if !strings.Contains(forced.Value, "forced") {
		t.Errorf("forced budget = %+v, want the forced marker", forced)
	}

	var list ConfigList
	decodeInto(t, mustRun(t, "", "config", "ls", "--json"), &list)
	found := 0
	for _, e := range list.Entries {
		if strings.HasPrefix(e.Key, resultBudgetPrefix) {
			found++
		}
	}
	if found != 2 {
		t.Errorf("config ls listed %d budget keys, want 2: %+v", found, list.Entries)
	}

	// "-" clears.
	mustRun(t, "", "config", "set", "resultBudget.github", "-")
	var cleared ConfigEntry
	decodeInto(t, mustRun(t, "", "config", "get", "resultBudget.github", "--json"), &cleared)
	if cleared.Value != "" {
		t.Errorf("cleared budget = %+v", cleared)
	}
	if code, _, _ := runCLI(t, "", "config", "set", "resultBudget.github", "-5"); code != ExitUsage {
		t.Errorf("negative budget exit = %d, want %d", code, ExitUsage)
	}
}
