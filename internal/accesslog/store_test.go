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
	s, err := Open(Options{Root: root, Key: testKey(), KeyID: "k1", Durability: DurabilitySync, MaxPackBytes: maxPack})
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
	bad := append([]byte(nil), testKey()...)
	bad[0]++
	if _, err := ReadPayload(root, ref, bad); err == nil {
		t.Fatal("wrong key decrypted the payload")
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
	if skipped != 0 || len(events) != 1 || events[0].BootID == "" || events[0].PID == 0 {
		t.Fatalf("events=%+v skipped=%d", events, skipped)
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
