package accesslog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func openTestStore(t *testing.T, maxPack int64) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	keyID, err := KeyID(testKey())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{Root: root, Key: testKey(), KeyID: keyID, Durability: DurabilitySync, MaxPackBytes: maxPack})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, root
}

func TestPayloadRoundTripIsExactAndEncrypted(t *testing.T) {
	t.Parallel()
	s, root := openTestStore(t, 0)
	ts := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	raw := []byte(` { "secret": "not plaintext", "n": 1 } `)
	ref, err := s.PutPayload(ts, "call-1", PayloadRequest, raw)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := os.ReadFile(filepath.Join(root, ref.Day, ref.File))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(packed, []byte("not plaintext")) {
		t.Fatal("payload appears in plaintext in the pack")
	}
	got, err := ReadPayload(root, ref, testKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("payload = %q, want exact %q", got, raw)
	}
	if err := VerifyPayload(root, ref, testKey(), "call-1", PayloadRequest); err != nil {
		t.Fatalf("VerifyPayload: %v", err)
	}
	if err := VerifyPayload(root, ref, testKey(), "other-call", PayloadRequest); err == nil {
		t.Fatal("payload passed with the wrong call binding")
	}
	bad := append([]byte(nil), testKey()...)
	bad[0]++
	if _, err := ReadPayload(root, ref, bad); err == nil {
		t.Fatal("wrong key decrypted the payload")
	}
}

func TestInspect(t *testing.T) {
	t.Parallel()
	s, root := openTestStore(t, 0)
	ts := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	ref, err := s.PutPayload(ts, "inspect-call", PayloadRequest, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{TS: ts, Kind: EventReceived, CallID: "inspect-call", Request: &ref}); err != nil {
		t.Fatal(err)
	}
	usage, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes <= 0 || usage.Days != 1 || usage.EventFiles != 1 || usage.PackFiles != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestPayloadPackRotates(t *testing.T) {
	t.Parallel()
	s, root := openTestStore(t, 300)
	ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var refs []PayloadRef
	for i := 0; i < 4; i++ {
		ref, err := s.PutPayload(ts, string(rune('a'+i)), PayloadRequest, bytes.Repeat([]byte{byte(i)}, 512))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	files, err := filepath.Glob(filepath.Join(root, refs[0].Day, "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("pack files = %d, want rotation", len(files))
	}
}

func TestHardCapacityBlocksBeforeWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	keyID, err := KeyID(testKey())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{
		Root: root, Key: testKey(), KeyID: keyID, Durability: DurabilityWrite,
		MaxBytes: 64, Clock: func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, err = s.PutPayload(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "too-large", PayloadRequest, []byte(`{"value":"larger than the limit after encryption"}`))
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("PutPayload error = %v, want ErrCapacity", err)
	}
	usage, inspectErr := Inspect(root)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if usage.Bytes > 64 {
		t.Fatalf("stored %d bytes above hard cap", usage.Bytes)
	}
}

func TestRetentionPrunesOnlyExpiredDayDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"2026-07-01", "2026-07-30", "2026-08-01", "not-a-day"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "marker"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	dry, err := Prune(root, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Days != 1 || len(dry.Names) != 1 || dry.Names[0] != "2026-07-01" {
		t.Fatalf("dry-run = %+v", dry)
	}
	removed, err := Prune(root, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Days != 1 || removed.Bytes == 0 {
		t.Fatalf("prune = %+v", removed)
	}
	for _, name := range []string{"2026-07-30", "2026-08-01", "not-a-day"} {
		if _, err := os.Stat(filepath.Join(root, name, "marker")); err != nil {
			t.Fatalf("protected %s: %v", name, err)
		}
	}
}

func TestEventRoundTripAndBound(t *testing.T) {
	t.Parallel()
	s, root := openTestStore(t, 0)
	ts := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	if err := s.Append(Event{TS: ts, Kind: EventReceived, CallID: "c", Client: "codex"}); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := ReadEvents(root)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(events) != 1 || events[0].BootID == "" || events[0].PID == 0 || events[0].MAC == "" {
		t.Fatalf("events=%+v skipped=%d", events, skipped)
	}
	if err := VerifyEvent(events[0], testKey()); err != nil {
		t.Fatalf("VerifyEvent: %v", err)
	}
	tampered := events[0]
	tampered.Client = "someone-else"
	if err := VerifyEvent(tampered, testKey()); err == nil {
		t.Fatal("tampered event passed authentication")
	}
	err = s.Append(Event{TS: ts, Kind: EventFinished, CallID: "c", Error: strings.Repeat("x", MaxEventLineBytes)})
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	lines, err := os.ReadFile(filepath.Join(root, utcDay(ts), EventFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(lines), []byte{'\n'}) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("torn event %q: %v", line, err)
		}
	}
}
