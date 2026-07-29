package audit

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestSavingsRecordGolden(t *testing.T) {
	r := SavingsRecord{
		TS:             time.Date(2026, 7, 26, 12, 0, 0, 123456789, time.UTC),
		Client:         "claude-code",
		Session:        "sess-1",
		Server:         "github",
		Mode:           "lazy-discovery",
		BaselineTokens: 12000,
		ActualTokens:   1800,
		SavedTokens:    10200,
	}
	const want = `{"ts":"2026-07-26T12:00:00.123456789Z","client":"claude-code",` +
		`"session":"sess-1","server":"github","mode":"lazy-discovery",` +
		`"baselineTokens":12000,"actualTokens":1800,"savedTokens":10200}`
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("golden mismatch:\n got %s\nwant %s", got, want)
	}

	// Optional grouping fields drop out; the token fields always appear.
	minimal, err := json.Marshal(SavingsRecord{
		TS:   time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		Mode: "shaping",
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantMinimal = `{"ts":"2026-07-26T12:00:00Z","mode":"shaping",` +
		`"baselineTokens":0,"actualTokens":0,"savedTokens":0}`
	if string(minimal) != wantMinimal {
		t.Errorf("minimal golden mismatch:\n got %s\nwant %s", minimal, wantMinimal)
	}
}

func TestSavingsStreamEndToEnd(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s, err := NewSavingsStream(filepath.Join(dir, SavingsFileName), WriterOptions{Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(SavingsRecord{Mode: "grouped", BaselineTokens: 100, ActualTokens: 40, SavedTokens: 60})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, SavingsFileName))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var r SavingsRecord
	if err := json.Unmarshal(lines[0], &r); err != nil {
		t.Fatal(err)
	}
	if !r.TS.Equal(clk.now) || r.SavedTokens != 60 {
		t.Errorf("round-trip mismatch: %+v", r)
	}
}

func TestStreamsOpenClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Audit.Append(Record{Actor: "client", Decision: DecisionAllowed})
	st.Savings.Append(SavingsRecord{Mode: "shaping"})
	if !st.Security.Emit(SecurityEvent{Event: "e.one", Severity: SeverityInfo}) {
		t.Error("first security emit must pass")
	}
	if st.Inspect.Enabled() {
		t.Error("inspect must start disabled")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{AuditFileName, SecurityFileName, SavingsFileName} {
		if got := len(readLines(t, filepath.Join(dir, f))); got != 1 {
			t.Errorf("%s has %d lines, want 1", f, got)
		}
	}
}
