package depguardtest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// probeTree materializes a disposable copy of the checkout for the probes
// to be written into, and returns its root.
//
// The probes used to be written into the real tree, which made this proof a
// concurrency hazard rather than a proof: `go test ./...` runs package test
// binaries in parallel, test/e2e's TestMain shells out to `go build
// ./cmd/agenthub` against that same tree, and a build that listed
// internal/platform between the probe's creation and its removal died with
// "open internal/platform/zz_depguard_probe_rule4.go: no such file or
// directory". Locally the window is narrow enough to hide (a warm build
// cache closes it in a second); on a CI runner with a cold cache the build
// spans the entire probe run and the failure is reliable. The real tree is
// now read-only for this package, so no builder — present or future — can
// be caught by it.
//
// The path is derived from the real root rather than random: golangci-lint
// caches by absolute file path, and a fresh directory per run would mean a
// cold lint of every probe every time. Reused across runs, private per
// checkout. Two concurrent runs over one checkout would fight over it, and
// would do so loudly (a vanished tree fails the lint, it does not quietly
// pass it).
func probeTree(t *testing.T, root string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(root))
	work := filepath.Join(os.TempDir(), "agenthub-depguard-tree-"+hex.EncodeToString(sum[:8]))
	if err := os.RemoveAll(work); err != nil {
		t.Fatalf("clearing previous probe tree %s: %v", work, err)
	}
	copyTree(t, root, work)
	for _, needed := range []string{".golangci.yml", "go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(work, needed)); err != nil {
			t.Fatalf("probe tree %s is missing %s: %v", work, needed, err)
		}
	}
	return work
}

// copyTree copies the Go module — sources and configuration — from src to
// dst. Build output and dependency directories are skipped: they are large
// (node_modules alone is 41M), and nothing golangci-lint does to a Go
// package needs them.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	skipAnywhere := map[string]bool{".git": true, "node_modules": true, ".task": true}
	skipAtRoot := map[string]bool{
		"bin": true, "dist": true, "tmp": true,
		".lintcache": true, ".make": true, ".agenthub": true, ".claude": true,
	}
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// A normal checkout has a .git directory, while a linked worktree has
		// a regular .git file pointing back to the main checkout. Neither is
		// part of the copied module: retaining the worktree pointer makes lint
		// resolve the disposable tree as the original checkout and then report
		// that the probe package has no Go files.
		if skipAnywhere[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rel == "." {
				return os.MkdirAll(dst, 0o755)
			}
			if skipAtRoot[rel] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		// Symlinks and other irregular entries are not copied: the module
		// contains none that a lint of a Go package reads, and following
		// one blindly is how a copy walks out of the tree.
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copying %s to probe tree %s: %v", src, dst, err)
	}
}

// writeProbe writes a violating (or clean) probe file and registers its
// removal. Removal is what lets each rule's control case lint the same
// package clean straight afterwards — the tree being disposable does not
// make it redundant. Probe files are named zz_depguard_probe_*.go and are
// also git-ignored as a second line of defense.
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
// docs/conventions.md#engineering-conventions for the four dependency constraints of docs/conventions.md#package-layout.
func TestDepguardRulesActuallyFire(t *testing.T) {
	root := repoRoot(t)
	bin := findGolangciLint(t, root)
	// Every probe below is written into `work`; `root` is read-only here.
	work := probeTree(t, root)
	t.Cleanup(func() { assertNoProbesIn(t, root) })

	// Rule 1: api (and cmd/agenthub-gui) must not import internal/*.
	t.Run("rule1_api_no_internal", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "api", "zz_depguard_probe_rule1.go"),
				"package api\n\n"+
					"// Probe: api must not import internal/* (docs/conventions.md#dependency-directions rule 1).\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/registry\"\n")
			out, err := runLint(t, bin, work, "api")
			assertBlocked(t, "1", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "api")
			assertClean(t, "1", out, err)
		})
	})

	// Rule 1 (second file set): cmd/agenthub-gui must not import internal/*.
	t.Run("rule1_gui_no_internal", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "cmd", "agenthub-gui", "zz_depguard_probe_rule1.go"),
				"package main\n\n"+
					"// Probe: cmd/agenthub-gui must not import internal/* (docs/conventions.md#dependency-directions rule 1).\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/registry\"\n")
			out, err := runLint(t, bin, work, "cmd/agenthub-gui")
			assertBlocked(t, "1-gui", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "cmd/agenthub-gui")
			assertClean(t, "1-gui", out, err)
		})
	})

	// Rule 2: internal/mcp may depend on the standard library only.
	// The probe imports cobra — present in go.mod, so it type-checks and
	// the only possible failure source is depguard's allowlist.
	t.Run("rule2_mcp_stdlib_only", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "internal", "mcp", "zz_depguard_probe_rule2.go"),
				"package mcp\n\n"+
					"// Probe: internal/mcp is stdlib-only (docs/conventions.md#dependency-directions rule 2, ruling #32).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, work, "internal/mcp")
			assertBlocked(t, "2", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "internal/mcp")
			assertClean(t, "2", out, err)
		})
	})

	// Rule 3: internal/pipeline must not import internal/ctlapi.
	// The rule was written before the package existed, so the test
	// still materializes the directory when the copy has none, and removes
	// it again if it did — the probe tree is disposable, but a control lint
	// of a directory this test invented has to be one it also cleaned up.
	t.Run("rule3_pipeline_no_ctlapi", func(t *testing.T) {
		dir := filepath.Join(work, "internal", "pipeline")
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
					"// control plane (docs/conventions.md#dependency-directions rule 3).\n"+
					"package pipeline\n\n"+
					"import _ \"github.com/dinstein/agent-hub/internal/ctlapi\"\n")
			out, err := runLint(t, bin, work, "internal/pipeline")
			assertBlocked(t, "3", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			// Control: same package location, no ctlapi import.
			writeProbe(t, filepath.Join(dir, "zz_depguard_probe_rule3_clean.go"),
				"// Package pipeline probe (control): no forbidden imports.\n"+
					"package pipeline\n")
			out, err := runLint(t, bin, work, "internal/pipeline")
			assertClean(t, "3", out, err)
		})
	})

	// Rule 4: internal/platform (representative of the zero-dependency
	// foundations platform/logx/guard) may depend on the stdlib only.
	t.Run("rule4_platform_zero_dep", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "internal", "platform", "zz_depguard_probe_rule4.go"),
				"package platform\n\n"+
					"// Probe: internal/platform is a zero-dependency foundation (docs/conventions.md#dependency-directions rule 4).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, work, "internal/platform")
			assertBlocked(t, "4", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "internal/platform")
			assertClean(t, "4", out, err)
		})
	})

	// Also cover logx with the same rule-4 shaped probe: it has its own
	// depguard rule (logx-zero-dep) that would silently rot if only
	// platform were exercised.
	t.Run("rule4_logx_zero_dep", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "internal", "logx", "zz_depguard_probe_rule4.go"),
				"package logx\n\n"+
					"// Probe: internal/logx is a zero-dependency foundation (docs/conventions.md#dependency-directions rule 4).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, work, "internal/logx")
			assertBlocked(t, "4-logx", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "internal/logx")
			assertClean(t, "4-logx", out, err)
		})
	})

	// Also cover internal/guard with the same rule-4 shaped probe: it has
	// its own depguard rule (guard-zero-dep) that would silently rot if
	// only platform and logx were exercised (docs/conventions.md#engineering-conventions).
	t.Run("rule4_guard_zero_dep", func(t *testing.T) {
		t.Run("violation_blocked", func(t *testing.T) {
			writeProbe(t, filepath.Join(work, "internal", "guard", "zz_depguard_probe_rule4.go"),
				"package guard\n\n"+
					"// Probe: internal/guard is a zero-dependency foundation (docs/conventions.md#dependency-directions rule 4).\n"+
					"import _ \"github.com/spf13/cobra\"\n")
			out, err := runLint(t, bin, work, "internal/guard")
			assertBlocked(t, "4-guard", out, err)
		})
		t.Run("clean_passes", func(t *testing.T) {
			out, err := runLint(t, bin, work, "internal/guard")
			assertClean(t, "4-guard", out, err)
		})
	})
}

// assertNoProbesIn fails if a probe file exists anywhere under dir. Run
// against the real checkout once the proof is over, it states the invariant
// that makes this package safe to run beside every other test package: the
// probes live in the copy, and a change that quietly moves one back into the
// tree turns a race in test/e2e into a failure here, where it is readable.
func assertNoProbesIn(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), "zz_depguard_probe_") {
			return fmt.Errorf("probe file %s was written into the real checkout; "+
				"probes belong in the disposable copy (see probeTree)", path)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// TestProbeNamingConventionIsIgnoredByGit guards the last line of defense:
// probes are written into a copy now, so one should never reach the tree —
// but if one ever does (a crashed run of an older revision, a change that
// moves them back), git must not pick it up. This only checks the
// .gitignore pattern textually — the test must not depend on a git binary
// being present.
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
