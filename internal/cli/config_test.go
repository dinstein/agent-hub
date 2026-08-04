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

// TestReleaseConfigLsWithholdsTheHTTPFace pins the reduced listing from both
// sides. A release build must not RECOMMEND the daemon's HTTP listener — it
// withholds every command that would then start, inspect or credential it —
// and a development build must still show all three, because that is where
// the face is worked on.
//
// Both output modes are asserted: `--json` is what a script reads, and a
// listing that dropped a key from the table but kept it in the envelope would
// be two answers to the same question.
func TestReleaseConfigLsWithholdsTheHTTPFace(t *testing.T) {
	setDataDir(t)

	code, out, stderr := runCLIReleaseHelp(t, "", "config", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("release `config ls` exit = %d, want %d (stderr %s)", code, ExitOK, stderr)
	}
	var release ConfigList
	decodeInto(t, out, &release)
	for _, e := range release.Entries {
		if strings.HasPrefix(e.Key, withheldKeyPrefix) {
			t.Errorf("release `config ls` lists the withheld key %q: %+v", e.Key, release.Entries)
		}
	}
	// The rest of the table is untouched — this withholds one family, it does
	// not empty the listing.
	if len(release.Entries) == 0 {
		t.Error("release `config ls` listed nothing at all")
	}
	if _, human, _ := runCLIReleaseHelp(t, "", "config", "ls"); strings.Contains(human, withheldKeyPrefix) {
		t.Errorf("release `config ls` human output still names the withheld keys:\n%s", human)
	}

	var dev ConfigList
	decodeInto(t, mustRun(t, "", "config", "ls", "--json"), &dev)
	found := 0
	for _, e := range dev.Entries {
		if strings.HasPrefix(e.Key, withheldKeyPrefix) {
			found++
		}
	}
	if found != 3 {
		t.Errorf("dev `config ls` listed %d %s* keys, want 3: %+v", found, withheldKeyPrefix, dev.Entries)
	}
}

// TestReleaseStillReadsAndWritesTheWithheldKeys is the load-bearing half:
// withholding a key from the listing must not become refusing it, exactly as
// hiding a command must not become disabling it. The GUI can store an address
// on this same installation, and a release CLI that could not read it back
// would report a hub's own listener as unset.
func TestReleaseStillReadsAndWritesTheWithheldKeys(t *testing.T) {
	setDataDir(t)

	code, _, stderr := runCLIReleaseHelp(t, "", "config", "set", "http.addr", "localhost:7777")
	if code != ExitOK {
		t.Fatalf("release `config set http.addr` exit = %d, want %d (stderr %s)", code, ExitOK, stderr)
	}
	code, out, stderr := runCLIReleaseHelp(t, "", "config", "get", "http.addr", "--json")
	if code != ExitOK {
		t.Fatalf("release `config get http.addr` exit = %d, want %d (stderr %s)", code, ExitOK, stderr)
	}
	var entry ConfigEntry
	decodeInto(t, out, &entry)
	if entry.Value != "localhost:7777" {
		t.Errorf("release `config get http.addr` = %+v, want the value just written", entry)
	}
}
