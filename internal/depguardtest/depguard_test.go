package depguardtest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot resolves the repository root from this package's directory
// (internal/depguardtest → ../..). Test binaries start in the package
// directory, so a relative walk is reliable without build-time tricks.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".golangci.yml")); err != nil {
		t.Fatalf("repo root %s does not contain .golangci.yml: %v", root, err)
	}
	return root
}

// findGolangciLint locates the golangci-lint binary: the
// AGENTHUB_GOLANGCI_LINT override first (when set it is authoritative —
// this also makes the skip path deterministically testable), then PATH,
// then the usual install locations. The proof cannot run without the
// binary, so the test skips with an actionable message instead of failing.
func findGolangciLint(t *testing.T, root string) string {
	t.Helper()
	if override := os.Getenv("AGENTHUB_GOLANGCI_LINT"); override != "" {
		if info, err := os.Stat(override); err == nil && !info.IsDir() {
			return override
		}
		t.Skipf("AGENTHUB_GOLANGCI_LINT=%s does not point to a binary; "+
			"install golangci-lint v2.12.2 there or unset the variable", override)
	}
	if p, err := exec.LookPath("golangci-lint"); err == nil {
		return p
	}
	var candidates []string
	if gp := goEnv(t, "GOPATH"); gp != "" {
		candidates = append(candidates, filepath.Join(gp, "bin", "golangci-lint"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "go", "bin", "golangci-lint"))
	}
	candidates = append(candidates,
		"/opt/homebrew/bin/golangci-lint",
		"/usr/local/bin/golangci-lint",
		filepath.Join(root, "bin", "golangci-lint"),
	)
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	t.Skipf("golangci-lint not found in PATH or %v; install v2.12.2 "+
		"(brew install golangci-lint, or the official install.sh) to run the depguard proof",
		candidates)
	return "" // unreachable
}

func goEnv(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lintCacheDir is a golangci-lint cache private to one checkout.
//
// The default cache is per-user and keyed by module path, which is identical
// in every worktree of this repository — so a run here can be served results
// that were computed in a sibling worktree, against files this one never had.
// When that sibling has since been removed, the cached issues arrive carrying
// its absolute paths and the control lint of an unmodified package "fails",
// which reads as the depguard rules being broken and is nothing of the sort.
//
// Keyed by root, so a worktree still reuses its own work run to run; two
// worktrees never share. Under $TMPDIR because it is disposable: losing it
// costs one slower lint, and the alternative failure costs an afternoon.
func lintCacheDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(os.TempDir(), "agenthub-golangci-"+hex.EncodeToString(sum[:8]))
}

// runLint runs golangci-lint over a single package path relative to the
// repo root. --allow-parallel-runners avoids lock contention with any
// other lint instance running concurrently (v2.12.2 flag, verified).
func runLint(t *testing.T, bin, root, relPkg string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, "run", "--allow-parallel-runners", "./"+relPkg+"/...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOLANGCI_LINT_CACHE="+lintCacheDir(root))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeProbe writes a violating (or clean) probe file and registers its
// removal. Probe files are named zz_depguard_probe_*.go and are also
// git-ignored as a second line of defense.
func writeProbe(t *testing.T, path, content string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("probe %s already exists; refusing to overwrite", path)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing probe %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("cleanup: removing probe %s: %v", path, err)
		}
	})
}

// assertBlocked asserts that lint failed and that the failure came from
// depguard (every probe type-checks, so nothing else can fail — but the
// explicit substring check keeps the proof honest).
func assertBlocked(t *testing.T, rule, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("rule %s: golangci-lint reported no issues for a violating probe\noutput:\n%s", rule, out)
	}
	if !strings.Contains(out, "depguard") {
		t.Fatalf("rule %s: lint failed but not via depguard\noutput:\n%s", rule, out)
	}
}

// assertClean asserts that lint passed with zero issues.
func assertClean(t *testing.T, rule, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("rule %s: control lint of the unmodified package failed\noutput:\n%s\nerror: %v", rule, out, err)
	}
}

// TestDepguardRulesActuallyFire is the failure-case proof required by
// canonical.md §6 for the four dependency constraints of canonical.md §2.
func TestDepguardRulesActuallyFire(t *testing.T) {
	root := repoRoot(t)
	bin := findGolangciLint(t, root)

	// Rule 1: api (and cmd/agenthub-gui) must not import internal/*.
	t.Run("rule1_api_no_internal", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(root, "api", "zz_depguard_probe_rule1.go"),
				"package api\n\n"+
					"// Probe: api must not import internal/* (canonical.md §2 rule 1).\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/registry\"\n")
			out, err := runLint(t, bin, root, "api")
			assertBlocked(t, "1", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, root, "api")
			assertClean(t, "1", out, err)
		})
	})

	// Rule 1 (second file set): cmd/agenthub-gui must not import internal/*.
	t.Run("rule1_gui_no_internal", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(root, "cmd", "agenthub-gui", "zz_depguard_probe_rule1.go"),
				"package main\n\n"+
					"// Probe: cmd/agenthub-gui must not import internal/* (canonical.md §2 rule 1).\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/registry\"\n")
			out, err := runLint(t, bin, root, "cmd/agenthub-gui")
			assertBlocked(t, "1-gui", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, root, "cmd/agenthub-gui")
			assertClean(t, "1-gui", out, err)
		})
	})

	// Rule 2: internal/mcp may depend on the standard library only.
	// The probe imports cobra — present in go.mod, so it type-checks and
	// the only possible failure source is depguard's allowlist.
	t.Run("rule2_mcp_stdlib_only", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(root, "internal", "mcp", "zz_depguard_probe_rule2.go"),
				"package mcp\n\n"+
					"// Probe: internal/mcp is stdlib-only (canonical.md §2 rule 2, ruling #32).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, root, "internal/mcp")
			assertBlocked(t, "2", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, root, "internal/mcp")
			assertClean(t, "2", out, err)
		})
	})

	// Rule 3: internal/pipeline must not import internal/ctlapi.
	// internal/pipeline does not exist yet (M0-8); the test materializes
	// it and removes the whole directory afterwards if it created it.
	t.Run("rule3_pipeline_no_ctlapi", func(t *testing.T) {
		dir := filepath.Join(root, "internal", "pipeline")
		created := false
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatalf("creating %s: %v", dir, err)
			}
			created = true
		}
		t.Cleanup(func() {
			if created {
				if err := os.RemoveAll(dir); err != nil {
					t.Errorf("cleanup: removing %s: %v", dir, err)
				}
			}
		})
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(dir, "zz_depguard_probe_rule3.go"),
				"// Package pipeline probe: the data plane must not import the\n"+
					"// control plane (canonical.md §2 rule 3).\n"+
					"package pipeline\n\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/ctlapi\"\n")
			out, err := runLint(t, bin, root, "internal/pipeline")
			assertBlocked(t, "3", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			// Control: same package location, no ctlapi import.
			writeProbe(t, filepath.Join(dir, "zz_depguard_probe_rule3_clean.go"),
				"// Package pipeline probe (control): no forbidden imports.\n"+
					"package pipeline\n")
			out, err := runLint(t, bin, root, "internal/pipeline")
			assertClean(t, "3", out, err)
		})
	})

	// Rule 4: internal/platform (representative of the zero-dependency
	// foundations platform/logx/guard) may depend on the stdlib only.
	t.Run("rule4_platform_zero_dep", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(root, "internal", "platform", "zz_depguard_probe_rule4.go"),
				"package platform\n\n"+
					"// Probe: internal/platform is a zero-dependency foundation (canonical.md §2 rule 4).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, root, "internal/platform")
			assertBlocked(t, "4", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, root, "internal/platform")
			assertClean(t, "4", out, err)
		})
	})

	// Also cover logx with the same rule-4 shaped probe: it has its own
	// depguard rule (logx-zero-dep) that would silently rot if only
	// platform were exercised.
	t.Run("rule4_logx_zero_dep", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(root, "internal", "logx", "zz_depguard_probe_rule4.go"),
				"package logx\n\n"+
					"// Probe: internal/logx is a zero-dependency foundation (canonical.md §2 rule 4).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, root, "internal/logx")
			assertBlocked(t, "4-logx", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, root, "internal/logx")
			assertClean(t, "4-logx", out, err)
		})
	})
}

// TestProbeNamingConventionIsIgnoredByGit guards the second line of
// defense: if a probe ever survives a crashed test run, git must not
// pick it up. This only checks the .gitignore pattern textually — the
// test must not depend on a git binary being present.
func TestProbeNamingConventionIsIgnoredByGit(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "zz_depguard_probe_*.go") {
		t.Fatal(".gitignore must contain the pattern zz_depguard_probe_*.go " +
			"so stray probe files can never be committed")
	}
}
