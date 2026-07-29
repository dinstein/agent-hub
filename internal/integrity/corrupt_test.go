package integrity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortRetries shrinks the corrupt-read retry delay for the duration of a
// test; the ladder itself (readRetries attempts) still runs in full.
func shortRetries(t *testing.T) {
	t.Helper()
	old := readRetryDelay
	readRetryDelay = time.Millisecond
	t.Cleanup(func() { readRetryDelay = old })
}

// Corrupt != Fresh, fail-closed (inherited invariant): a state file that
// exists but cannot be parsed must fail every operation with
// ErrStoreCorrupt, must never be treated as an empty set, must never be
// renamed aside, and must never be overwritten.
func TestCorruptStoreFailsClosed(t *testing.T) {
	shortRetries(t)
	ctx := context.Background()

	corruptions := map[string]string{
		"garbage":             `{"version": 1, "pins": <<<not json>>>`,
		"empty file":          "",
		"trailing data":       `{"version":1} trailing`,
		"unsupported version": `{"version": 99}`,
	}

	for label, content := range corruptions {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{pinsFileName, quarantineFileName, approvalsFileName} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			pinStore, err := OpenPinStore(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			qStore, err := OpenQuarantineStore(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			aStore, err := OpenApprovalStore(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}

			ops := map[string]func() error{
				"pins.CheckServer": func() error {
					_, err := pinStore.CheckServer(ctx, "srv", []ToolSnapshot{{Name: "t"}})
					return err
				},
				"pins.Rebaseline": func() error {
					_, err := pinStore.Rebaseline(ctx, "srv", "t", ToolSnapshot{Name: "t"})
					return err
				},
				"pins.Pins": func() error { _, err := pinStore.Pins(ctx); return err },
				"quarantine.Add": func() error {
					return qStore.Add(ctx, "srv__t", QuarantineEntry{Server: "srv", Tool: "t"})
				},
				"quarantine.Release": func() error {
					_, _, err := qStore.Release(ctx, "srv__t")
					return err
				},
				"quarantine.IsQuarantined": func() error {
					_, err := qStore.IsQuarantined(ctx, "srv__t")
					return err
				},
				"approvals.Observe": func() error {
					_, err := aStore.Observe(ctx, "srv", ToolSnapshot{Name: "t"}, ModeAuto)
					return err
				},
				"approvals.Approve": func() error {
					_, err := aStore.Approve(ctx, "srv", "t")
					return err
				},
				"approvals.Get": func() error {
					_, err := aStore.Get(ctx, "srv", "t")
					return err
				},
			}
			for opName, op := range ops {
				err := op()
				if !errors.Is(err, ErrStoreCorrupt) {
					t.Errorf("%s on corrupt store: err = %v, want ErrStoreCorrupt", opName, err)
				}
				if errors.Is(err, ErrNotFound) {
					t.Errorf("%s conflated corrupt store with missing record", opName)
				}
			}

			// The corrupt files are untouched: same content, no renames, no
			// extra files beyond the lock siblings.
			for _, name := range []string{pinsFileName, quarantineFileName, approvalsFileName} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("%s vanished after fail-closed ops: %v", name, err)
				}
				if string(got) != content {
					t.Errorf("%s was modified after fail-closed ops", name)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				n := e.Name()
				switch n {
				case pinsFileName, quarantineFileName, approvalsFileName,
					pinsFileName + lockSuffix, quarantineFileName + lockSuffix, approvalsFileName + lockSuffix:
				default:
					t.Errorf("unexpected file %q created beside corrupt store (rename/quarantine forbidden)", n)
				}
			}
		})
	}
}

// A missing file IS fresh (first run): the one case that legitimately reads
// as an empty store.
func TestMissingFileIsFresh(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pinStore, err := OpenPinStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := pinStore.Pins(ctx)
	if err != nil {
		t.Fatalf("Pins on fresh store: %v", err)
	}
	if len(pins) != 0 {
		t.Errorf("fresh store has %d pins", len(pins))
	}
	qStore, err := OpenQuarantineStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if q, err := qStore.IsQuarantined(ctx, "x"); err != nil || q {
		t.Errorf("fresh quarantine: %v %v", q, err)
	}
}

// Observe on a corrupt store must not create or overwrite records — the
// 7.5 sentinel distinction: a decode error treated as "record missing" would
// let auto-approval clobber a Pending record.
func TestCorruptStoreNeverOverwritten(t *testing.T) {
	shortRetries(t)
	ctx := context.Background()
	dir := t.TempDir()
	aStore, err := OpenApprovalStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aStore.Observe(ctx, "srv", ToolSnapshot{Name: "t"}, ModeManual); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, approvalsFileName)
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := aStore.Observe(ctx, "srv", ToolSnapshot{Name: "t"}, ModeAuto); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("Observe on corrupt store: %v, want ErrStoreCorrupt", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{corrupt" {
		t.Error("corrupt approvals file was overwritten by Observe (Pending record could be clobbered)")
	}
}
