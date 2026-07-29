package shaping

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// goldenNotFound is the ONE response every fetch_result failure produces.
// Frozen: an agent (or an attacker walking the guessable id space) must not
// be able to tell "expired" from "not yours" from "never existed".
const goldenNotFound = `[{"type":"text","text":"fetch_result: unknown or expired cursor. ` +
	`Re-run the original tool call to obtain a fresh result and cursor."}]`

// retained builds a store holding one entry owned by goldenOwner.
func retained(t *testing.T, full string) (*MemStore, Entry) {
	t.Helper()
	s := NewMemStore(0)
	s.Clock = func() time.Time { return goldenNow }
	e := Entry{
		ID: goldenID, Owner: goldenOwner, CreatedAt: goldenNow,
		TTL: 30 * time.Minute, Budget: Budget{Bytes: 256}, Full: full,
	}
	if err := s.Put(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	return s, e
}

// Owner verification is the only isolation fetch_result has, and every way
// of failing it must be indistinguishable (docs/flows.md).
func TestFetchMissesAreIndistinguishable(t *testing.T) {
	store, _ := retained(t, strings.Repeat("secret payload. ", 100))

	expired := NewMemStore(0)
	expired.Clock = func() time.Time { return goldenNow.Add(time.Hour) }
	if err := expired.Put(t.Context(), Entry{
		ID: goldenID, Owner: goldenOwner, CreatedAt: goldenNow,
		TTL: time.Minute, Budget: Budget{Bytes: 256}, Full: "gone",
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		store  Store
		owner  Owner
		cursor string
	}{
		{"another session's cursor", store, "claude-code:2", goldenID},
		{"empty owner", store, "", goldenID},
		{"owner prefix", store, "claude-code:", goldenID},
		{"unknown cursor", store, goldenOwner, "rc-000999"},
		{"malformed cursor", store, goldenOwner, "../../etc/passwd"},
		{"empty cursor", store, goldenOwner, ""},
		{"expired cursor", expired, goldenOwner, goldenID},
		{"nil store", nil, goldenOwner, goldenID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Fetch(t.Context(), tc.store, tc.owner, tc.cursor, 0)
			if ok {
				t.Fatal("miss must not report success")
			}
			if !res.IsError {
				t.Error("miss must be an isError result")
			}
			if got := string(res.Content); got != goldenNotFound {
				t.Errorf("miss text drifted:\n got %s\nwant %s", got, goldenNotFound)
			}
			if strings.Contains(string(res.Content), "secret payload") {
				t.Error("miss leaked retained content")
			}
		})
	}
}

// The rightful owner still gets the data — the isolation must not be
// achieved by denying everyone.
func TestFetchServesOwner(t *testing.T) {
	full := strings.Repeat("payload. ", 60)
	store, _ := retained(t, full)
	res, ok := Fetch(t.Context(), store, goldenOwner, goldenID, 0)
	if !ok {
		t.Fatal("owner must be served")
	}
	if res.IsError {
		t.Error("a served page is not an error result")
	}
	if got := blockText(t, res.Content, 0); !strings.HasPrefix(full, got) || got == "" {
		t.Errorf("served text is not a payload prefix: %q", got)
	}
}

// A negative offset clamps to 0; an offset at or past the end is a
// successful empty page (end of stream), not a miss — the data was
// delivered, there is simply nothing left.
func TestFetchOffsetEdges(t *testing.T) {
	full := "abcdefghij"
	store, _ := retained(t, full)
	neg, ok := Fetch(t.Context(), store, goldenOwner, goldenID, -5)
	if !ok || blockText(t, neg.Content, 0) != full {
		t.Errorf("negative offset must clamp to 0, got %s", neg.Content)
	}
	for _, off := range []int{len(full), len(full) + 1000} {
		end, ok := Fetch(t.Context(), store, goldenOwner, goldenID, off)
		if !ok {
			t.Fatalf("offset %d must succeed as end-of-stream", off)
		}
		if string(end.Content) != `[]` {
			t.Errorf("offset %d = %s, want []", off, end.Content)
		}
		if end.IsError {
			t.Errorf("offset %d must not be an error", off)
		}
	}
}

// A budget too small for even one character must still advance: a page that
// can never move forward is a livelock.
func TestFetchAlwaysAdvances(t *testing.T) {
	s := NewMemStore(0)
	s.Clock = func() time.Time { return goldenNow }
	if err := s.Put(t.Context(), Entry{
		ID: goldenID, Owner: goldenOwner, CreatedAt: goldenNow, TTL: time.Hour,
		Budget: Budget{Bytes: 1}, Full: "\U0001F600\U0001F600\U0001F600",
	}); err != nil {
		t.Fatal(err)
	}
	offset := 0
	for i := 0; i < 10 && offset < 3; i++ {
		res, ok := Fetch(t.Context(), s, goldenOwner, goldenID, offset)
		if !ok {
			t.Fatal("unexpected miss")
		}
		text := blockText(t, res.Content, 0)
		if text == "" {
			t.Fatalf("no progress at offset %d", offset)
		}
		offset += len([]rune(text))
	}
	if offset != 3 {
		t.Fatalf("pagination stalled at %d", offset)
	}
}

// An unbounded budget delivers the whole remainder in one page, with no
// trailer.
func TestFetchUnboundedBudget(t *testing.T) {
	full := strings.Repeat("x", 5000)
	s := NewMemStore(0)
	s.Clock = func() time.Time { return goldenNow }
	if err := s.Put(t.Context(), Entry{
		ID: goldenID, Owner: goldenOwner, CreatedAt: goldenNow, TTL: time.Hour,
		Budget: Budget{}, Full: full,
	}); err != nil {
		t.Fatal(err)
	}
	res, ok := Fetch(t.Context(), s, goldenOwner, goldenID, 0)
	if !ok {
		t.Fatal("unexpected miss")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 (no trailer)", len(blocks))
	}
	if blockText(t, res.Content, 0) != full {
		t.Error("unbounded fetch truncated the payload")
	}
}

// A cancelled context must not be reported as a cursor miss to the agent...
// but it must not serve data either. Fetch collapses it into the same
// not-found response: the caller's ctx error is already the real signal.
func TestFetchCancelledContext(t *testing.T) {
	store, _ := retained(t, "payload")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, ok := Fetch(ctx, store, goldenOwner, goldenID, 0)
	if ok {
		t.Fatal("cancelled fetch must not serve")
	}
	if string(res.Content) != goldenNotFound {
		t.Errorf("cancelled fetch text drifted: %s", res.Content)
	}
}

// Store contract: every failure mode returns ErrNotFound and nothing more
// specific.
func TestStoreGetErrorIsUniform(t *testing.T) {
	store, _ := retained(t, "payload")
	for _, tc := range []struct {
		owner  Owner
		cursor string
	}{
		{"claude-code:2", goldenID},
		{goldenOwner, "rc-000042"},
		{goldenOwner, "bogus"},
	} {
		if _, err := store.Get(t.Context(), tc.owner, tc.cursor); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q,%q) err = %v, want ErrNotFound", tc.owner, tc.cursor, err)
		}
	}
}

// Retain on a zero cursor is a no-op so callers can retain unconditionally.
func TestRetainZeroCursor(t *testing.T) {
	s := NewMemStore(0)
	if err := Retain(t.Context(), s, Cursor{}); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Errorf("zero cursor stored %d entries", s.Len())
	}
	if err := Retain(t.Context(), nil, Cursor{ID: goldenID}); err != nil {
		t.Errorf("nil store: %v", err)
	}
}
