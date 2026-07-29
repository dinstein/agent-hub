package cli

import (
	"os"
	"path/filepath"
	"testing"
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
		t.Errorf("detect must always report the directly writable clients")
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
