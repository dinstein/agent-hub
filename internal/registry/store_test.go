package registry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func mustOpen(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return st
}

func addServer(t *testing.T, st *Store, name, command string) {
	t.Helper()
	err := st.Update(context.Background(), func(tx *Tx) error {
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]Doc[ServerEntry]{}
		}
		tx.Servers.V.Servers[name] = Doc[ServerEntry]{V: ServerEntry{
			Transport: "stdio", Command: command, Enabled: true, Source: "test",
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestOpenInitializesAllDocuments(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)

	for _, kind := range []DocKind{DocMeta, DocServers, DocProfiles, DocClients, DocGovernance} {
		p := docPath(dir, kind)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s not created: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has perm %o, want 0600", p, perm)
		}
	}
	snap := st.Snapshot()
	if snap.Generation != 0 {
		t.Errorf("fresh registry generation = %d, want 0", snap.Generation)
	}
	if snap.Servers.V.Servers == nil || len(snap.Servers.V.Servers) != 0 {
		t.Errorf("fresh servers doc = %+v, want empty map", snap.Servers.V)
	}
}

func TestUpdatePersistsAndBumpsGeneration(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)

	addServer(t, st, "fs", "npx")
	if g := st.Snapshot().Generation; g != 1 {
		t.Fatalf("generation after first write = %d, want 1", g)
	}

	// A second, independent Store must see the committed state.
	st2 := mustOpen(t, dir)
	snap := st2.Snapshot()
	if snap.Generation != 1 {
		t.Errorf("second store generation = %d, want 1", snap.Generation)
	}
	if got := snap.Servers.V.Servers["fs"].V.Command; got != "npx" {
		t.Errorf("server command = %q, want npx", got)
	}
}

func TestNoOpUpdateSkipsWriteAndBump(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	addServer(t, st, "fs", "npx")

	// Rewrite the same value: parsed-JSON comparison must classify this as
	// a no-op — no generation bump, no new backup.
	err := st.Update(context.Background(), func(tx *Tx) error {
		e := tx.Servers.V.Servers["fs"]
		e.V.Command = "npx" // unchanged
		tx.Servers.V.Servers["fs"] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if g := st.Snapshot().Generation; g != 1 {
		t.Errorf("generation after no-op = %d, want 1", g)
	}
	if _, err := os.Stat(filepath.Join(dir, backupsDirName, "servers.json.2")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no-op update produced a backup generation: %v", err)
	}
}

func TestBackupRotationKeepsFiveGenerations(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)

	// 7 real writes; capture servers.json content after each.
	var contents [][]byte
	for i := 0; i < 7; i++ {
		addServer(t, st, "fs", fmt.Sprintf("cmd-%d", i))
		b, err := os.ReadFile(docPath(dir, DocServers))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, b)
	}

	slot := func(i int) string {
		return filepath.Join(dir, backupsDirName, fmt.Sprintf("servers.json.%d", i))
	}
	for i := 1; i <= backupDepth; i++ {
		if _, err := os.Stat(slot(i)); err != nil {
			t.Fatalf("backup slot %d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(slot(backupDepth + 1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup depth exceeded %d", backupDepth)
	}
	// Slot 1 holds the content replaced by the last write = state after write 6.
	b1, err := os.ReadFile(slot(1))
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(contents[5]) {
		t.Errorf("backup .1 is not the previous generation:\n got: %s\nwant: %s", b1, contents[5])
	}
	// Slot 5 holds the state after write 2.
	b5, err := os.ReadFile(slot(backupDepth))
	if err != nil {
		t.Fatal(err)
	}
	if string(b5) != string(contents[1]) {
		t.Errorf("backup .5 is not 5 generations back:\n got: %s\nwant: %s", b5, contents[1])
	}
}

func TestUnknownFieldPassthroughGolden(t *testing.T) {
	dir := t.TempDir()
	input := `{
  "future_top": {"a": 1},
  "servers": {
    "alpha": {
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem"],
      "env": {"TOKEN": "${SECRET_ALPHA}"},
      "enabled": true,
      "future_entry": {"nested": ["x"]}
    }
  }
}
`
	if err := os.WriteFile(docPath(dir, DocServers), []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	st := mustOpen(t, dir)
	err := st.Update(context.Background(), func(tx *Tx) error {
		e := tx.Servers.V.Servers["alpha"]
		e.V.Enabled = false // edit a known field; unknown fields must survive
		tx.Servers.V.Servers["alpha"] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(docPath(dir, DocServers))
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "servers.golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to regenerate): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("servers.json does not match golden\n got: %s\nwant: %s", got, want)
	}
}

func TestUnreadableFileIsQuarantinedNotDestroyed(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, dir) // create valid files

	garbage := "{this is not json"
	if err := os.WriteFile(docPath(dir, DocProfiles), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if st == nil {
		t.Fatal("store must remain usable when one document is unreadable")
	}
	var uerr *UnreadableError
	if !errors.As(err, &uerr) {
		t.Fatalf("Open error = %v, want *UnreadableError", err)
	}
	if uerr.Kind != DocProfiles {
		t.Errorf("quarantined kind = %s, want profiles", uerr.Kind)
	}
	qb, rerr := os.ReadFile(uerr.QuarantinePath)
	if rerr != nil {
		t.Fatalf("quarantine file missing: %v", rerr)
	}
	if string(qb) != garbage {
		t.Errorf("quarantine content = %q, want original garbage", qb)
	}
	// profiles.json must be recreated with defaults, and other docs must work.
	if _, err := os.Stat(docPath(dir, DocProfiles)); err != nil {
		t.Errorf("profiles.json not recreated: %v", err)
	}
	addServer(t, st, "fs", "npx")
	if got := st.Snapshot().Servers.V.Servers["fs"].V.Command; got != "npx" {
		t.Errorf("other documents blocked by quarantine: %q", got)
	}
}

func TestUpdateSurvivesQuarantineOfUnrelatedDoc(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)

	if err := os.WriteFile(docPath(dir, DocClients), []byte("###"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := st.Update(context.Background(), func(tx *Tx) error {
		tx.Servers.V.Servers["fs"] = Doc[ServerEntry]{V: ServerEntry{Transport: "stdio", Command: "npx", Enabled: true}}
		return nil
	})
	var uerr *UnreadableError
	if !errors.As(err, &uerr) || uerr.Kind != DocClients {
		t.Fatalf("Update error = %v, want *UnreadableError for clients", err)
	}
	// The servers write must have been committed despite the quarantine.
	st2 := mustOpen(t, dir)
	if got := st2.Snapshot().Servers.V.Servers["fs"].V.Command; got != "npx" {
		t.Errorf("update lost due to unrelated quarantine: %q", got)
	}
	if g := st2.Snapshot().Generation; g != 1 {
		t.Errorf("generation = %d, want 1", g)
	}
}

func TestLockTimeoutReturnsTypedError(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenOptions(dir, Options{LockTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the flock on a separate descriptor; flock excludes across fds
	// even within one process.
	f, err := os.OpenFile(dirLockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := flockExclusiveNB(f); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = st.Update(context.Background(), func(*Tx) error { return nil })
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("Update error = %v, want ErrLockTimeout", err)
	}
	var lerr *LockTimeoutError
	if !errors.As(err, &lerr) {
		t.Fatalf("error is not *LockTimeoutError: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned before the timeout elapsed: %s", elapsed)
	}
}

func TestUpdateHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenOptions(dir, Options{LockTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dirLockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := flockExclusiveNB(f); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = st.Update(ctx, func(*Tx) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Update error = %v, want context.DeadlineExceeded", err)
	}
}

func TestFnErrorAbortsWithoutWrite(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	boom := errors.New("boom")
	err := st.Update(context.Background(), func(tx *Tx) error {
		tx.Servers.V.Servers["fs"] = Doc[ServerEntry]{V: ServerEntry{Transport: "stdio", Command: "npx"}}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	st2 := mustOpen(t, dir)
	if g := st2.Snapshot().Generation; g != 0 {
		t.Errorf("generation = %d after aborted update, want 0", g)
	}
	if n := len(st2.Snapshot().Servers.V.Servers); n != 0 {
		t.Errorf("aborted update leaked %d servers to disk", n)
	}
}

func TestReadRetryRidesOutTransientCorruption(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, dir)

	// Simulate a non-atomic writer caught mid-write: servers.json is invalid
	// now, becomes valid ~150ms in (within the 4x75ms retry window).
	if err := os.WriteFile(docPath(dir, DocServers), []byte(`{"servers": {"x": `), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		valid := `{"servers": {"x": {"transport": "stdio", "command": "ok", "enabled": true}}}`
		tmp := docPath(dir, DocServers) + ".ext"
		if err := os.WriteFile(tmp, []byte(valid), 0o600); err != nil {
			return
		}
		_ = os.Rename(tmp, docPath(dir, DocServers))
	}()

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open should have succeeded via read retry, got: %v", err)
	}
	if got := st.Snapshot().Servers.V.Servers["x"].V.Command; got != "ok" {
		t.Errorf("server command = %q, want ok", got)
	}
}

func TestEnvSecretPlaceholdersStoredVerbatim(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	err := st.Update(context.Background(), func(tx *Tx) error {
		tx.Servers.V.Servers["s"] = Doc[ServerEntry]{V: ServerEntry{
			Transport: "stdio", Command: "run", Enabled: true,
			Env: map[string]string{"API_KEY": "${SECRET_API_KEY}"},
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st2 := mustOpen(t, dir)
	if got := st2.Snapshot().Servers.V.Servers["s"].V.Env["API_KEY"]; got != "${SECRET_API_KEY}" {
		t.Errorf("placeholder not stored verbatim: %q", got)
	}
}
