package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// TestClientDetect runs against an isolated HOME so the result is
// deterministic and no test reads the developer's real client
// configuration.
func TestClientDetect(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	var empty DetectList
	decodeInto(t, mustRun(t, "", "client", "detect", "--json"), &empty)
	if len(empty.Found) != 0 {
		t.Fatalf("found = %+v, want nothing in an empty home", empty.Found)
	}
	if len(empty.Supported) == 0 {
		t.Errorf("detect must always report the clients it supports")
	}

	// A user-level Claude Desktop configuration should now be detected.
	cfgDir := filepath.Join(home, "Library", "Application Support", "Claude")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linuxDir := filepath.Join(home, ".config", "Claude")
	if err := os.MkdirAll(linuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"mcpServers":{"linear":{"command":"npx","args":["-y","linear-mcp"]}}}`)
	for _, dir := range []string{cfgDir, linuxDir} {
		if err := os.WriteFile(filepath.Join(dir, "claude_desktop_config.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var found DetectList
	decodeInto(t, mustRun(t, "", "client", "detect", "--json"), &found)
	hit := false
	for _, d := range found.Found {
		if d.Client == "claude-desktop" {
			hit = true
			if d.Size == 0 || d.Modified == "" {
				t.Errorf("detected row lacks stat data: %+v", d)
			}
		}
	}
	if !hit {
		t.Errorf("claude-desktop not detected: %+v", found.Found)
	}
}

// TestClientDetectDoesNotCallEveryClientWritable holds the footer to the
// table above it. `supported` is every client agenthub knows about, and it
// was being LABELLED "directly writable clients" — so codex was named as
// writable on the same screen as its own row reading WRITABLE=no, and the
// row is the one that looked wrong.
//
// The split is asserted against internal/clients rather than a list written
// here: a new read-only client must land in `indirect` by being added to the
// table, not by someone remembering to update a test.
func TestClientDetectDoesNotCallEveryClientWritable(t *testing.T) {
	setDataDir(t)
	t.Setenv("HOME", t.TempDir())

	var list DetectList
	decodeInto(t, mustRun(t, "", "client", "detect", "--json"), &list)

	var want []string
	for _, f := range clients.Default().Formats() {
		if !f.Writable() {
			want = append(want, f.ID())
		}
	}
	if len(want) == 0 {
		t.Fatal("no read-only client shapes left; this test has nothing to hold")
	}
	if !slices.Equal(list.Indirect, want) {
		t.Errorf("indirect = %v, want %v", list.Indirect, want)
	}
	for _, id := range list.Indirect {
		if !slices.Contains(list.Supported, id) {
			t.Errorf("%q is indirect but missing from supported %v", id, list.Supported)
		}
	}

	// The human footer must not re-introduce the claim the field names avoid.
	_, out, _ := runCLI(t, "", "client", "detect")
	if strings.Contains(out, "directly writable clients:") {
		t.Errorf("footer calls every supported client writable:\n%s", out)
	}
	if !strings.Contains(out, "does not write these itself") {
		t.Errorf("footer does not name the read-only clients:\n%s", out)
	}
}
