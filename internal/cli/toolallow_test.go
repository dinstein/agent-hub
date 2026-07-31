package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeToolAllow(t *testing.T, out string) ToolAllowResult {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var res ToolAllowResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("data is not a tool-allow result: %v\n%s", err, env.Data)
	}
	return res
}

// The three states have to be distinguishable ON THE WIRE, not just in the
// registry: null is "no rule", [] is "nothing", and a consumer that cannot
// tell them apart reads block-all as allow-all.
func TestToolAllowThreeStatesOverJSON(t *testing.T) {
	seedCatalog(t)

	code, out, stderr := runCLI(t, "", "tool", "allow", "fs", "--only", "read_file", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := decodeToolAllow(t, out).Tools; len(got) != 1 || got[0] != "read_file" {
		t.Errorf("--only tools = %v, want [read_file]", got)
	}

	code, out, stderr = runCLI(t, "", "tool", "allow", "fs", "--none", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := decodeToolAllow(t, out).Tools; got == nil || len(got) != 0 {
		t.Errorf("--none tools = %v, want an empty list and NOT null", got)
	}
	// The raw bytes, because a `[]` that marshals as `null` is exactly the
	// regression the struct comparison above cannot see.
	if !strings.Contains(out, `"tools":[]`) {
		t.Errorf("--none must put an empty array on the wire, got %s", out)
	}

	code, out, stderr = runCLI(t, "", "tool", "allow", "fs", "--all", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := decodeToolAllow(t, out).Tools; got != nil {
		t.Errorf("--all tools = %v, want null (no rule at all)", got)
	}
	if !strings.Contains(out, `"tools":null`) {
		t.Errorf("--all must put null on the wire, got %s", out)
	}
}

// A bare `tool allow fs` used to mean "expose NOTHING from fs" — one
// forgotten argument away from the opposite of the intent, and silent. It is
// now a usage error naming all three ways out.
func TestToolAllowRefusesAnUnspecifiedEdit(t *testing.T) {
	seedCatalog(t)

	code, _, stderr := runCLI(t, "", "tool", "allow", "fs")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--only") || !strings.Contains(stderr, "--none") {
		t.Errorf("the error must name the ways out, got %s", stderr)
	}
	// And nothing may have been written on the way to refusing.
	_, out, _ := runCLI(t, "", "server", "ls", "--json")
	if strings.Contains(out, `"tools"`) {
		t.Errorf("a refused edit still wrote a rule: %s", out)
	}

	code, _, stderr = runCLI(t, "", "tool", "allow", "fs", "--all", "--none")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error for two modes at once; stderr = %s", code, stderr)
	}
}

// The spelling cross-check is the only thing standing between a typo and a
// server that silently offers nothing, and it must be identical to the one
// `profile tools` runs — same helper, same sentence.
func TestToolAllowWarnsOnAnUnknownToolName(t *testing.T) {
	seedCatalog(t)

	code, out, stderr := runCLI(t, "", "tool", "allow", "fs", "--only", "read_fil", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	env := decodeEnvelope(t, out)
	if !strings.Contains(strings.Join(env.Warnings, " "), "no recorded tool named") {
		t.Errorf("warnings = %v, want the unknown-tool warning", env.Warnings)
	}
	// Warned, not refused: the rule is stored, because the cache may simply
	// be colder than the operator's knowledge of the server.
	if got := decodeToolAllow(t, out).Tools; len(got) != 1 || got[0] != "read_fil" {
		t.Errorf("the rule must be stored as typed, got %v", got)
	}
}
