package audit

import (
	"strconv"
	"strings"
	"testing"
)

func TestInspectOptIn(t *testing.T) {
	r := NewInspectRing()
	if r.Enabled() {
		t.Fatal("ring must start disabled (opt-in)")
	}
	if r.Add(InspectEntry{Kind: "request", Body: "x"}) {
		t.Error("Add while disabled must be a no-op")
	}
	if r.Len() != 0 {
		t.Errorf("len = %d, want 0", r.Len())
	}
	r.SetEnabled(true)
	if !r.Add(InspectEntry{Kind: "request", Body: "x"}) {
		t.Error("Add while enabled must retain")
	}
	if r.Len() != 1 {
		t.Errorf("len = %d, want 1", r.Len())
	}
}

func TestInspectDisableClears(t *testing.T) {
	r := NewInspectRing()
	r.SetEnabled(true)
	for i := 0; i < 10; i++ {
		r.Add(InspectEntry{Kind: "request", Body: "b"})
	}
	r.SetEnabled(false)
	if r.Len() != 0 || len(r.Snapshot()) != 0 {
		t.Error("disable must clear every buffered entry")
	}
}

func TestInspectRingWrap(t *testing.T) {
	r := NewInspectRing()
	r.SetEnabled(true)
	const total = InspectCapacity + 20
	for i := 0; i < total; i++ {
		r.Add(InspectEntry{Kind: "request", Body: strconv.Itoa(i)})
	}
	snap := r.Snapshot()
	if len(snap) != InspectCapacity {
		t.Fatalf("snapshot len = %d, want %d", len(snap), InspectCapacity)
	}
	// Oldest-first, holding the last InspectCapacity entries with
	// monotonically increasing Seq.
	for i, e := range snap {
		wantBody := strconv.Itoa(total - InspectCapacity + i)
		if e.Body != wantBody {
			t.Errorf("snap[%d].Body = %q, want %q", i, e.Body, wantBody)
		}
		if i > 0 && e.Seq != snap[i-1].Seq+1 {
			t.Errorf("snap[%d].Seq = %d, not contiguous after %d", i, e.Seq, snap[i-1].Seq)
		}
	}
	if snap[len(snap)-1].Seq != total {
		t.Errorf("last Seq = %d, want %d (Seq survives eviction)", snap[len(snap)-1].Seq, total)
	}
}

func TestInspectBodyTruncation(t *testing.T) {
	r := NewInspectRing()
	r.SetEnabled(true)
	big := strings.Repeat("a", InspectMaxBody+500)
	r.Add(InspectEntry{Kind: "response", Body: big})
	e := r.Snapshot()[0]
	if !e.Truncated {
		t.Error("oversized body must be flagged truncated")
	}
	if len(e.Body) != InspectMaxBody {
		t.Errorf("body len = %d, want %d", len(e.Body), InspectMaxBody)
	}
	if e.OrigBytes != len(big) {
		t.Errorf("origBytes = %d, want %d", e.OrigBytes, len(big))
	}

	// Truncation lands on a UTF-8 boundary: a multi-byte rune straddling
	// the cut is dropped, not left as invalid bytes.
	multi := strings.Repeat("界", (InspectMaxBody/3)+10) // 3 bytes each
	r.Add(InspectEntry{Kind: "response", Body: multi})
	e = r.Snapshot()[1]
	if !e.Truncated || !strings.HasSuffix(e.Body, "界") {
		t.Errorf("multi-byte truncation invalid: truncated=%v len=%d", e.Truncated, len(e.Body))
	}
	for _, ru := range e.Body {
		if ru == '�' {
			t.Fatal("truncated body contains replacement rune (invalid UTF-8 leaked)")
		}
	}

	small := InspectEntry{Kind: "request", Body: "ok"}
	r.Add(small)
	e = r.Snapshot()[2]
	if e.Truncated || e.Body != "ok" || e.OrigBytes != 2 {
		t.Errorf("small body altered: %+v", e)
	}
}

func TestInspectSnapshotIsCopy(t *testing.T) {
	r := NewInspectRing()
	r.SetEnabled(true)
	r.Add(InspectEntry{Kind: "request", Body: "orig"})
	snap := r.Snapshot()
	snap[0].Body = "mutated"
	if r.Snapshot()[0].Body != "orig" {
		t.Error("Snapshot must return a copy")
	}
}
