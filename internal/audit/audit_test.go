package audit

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestAuditRecordGolden freezes the serialized byte layout of one line —
// field order and presence are contract for CSV export and line-oriented
// consumers ("determinism is contract").
func TestAuditRecordGolden(t *testing.T) {
	r := Record{
		TS:        time.Date(2026, 7, 26, 12, 0, 0, 123456789, time.UTC),
		Actor:     "client",
		Client:    "claude-code",
		Session:   "sess-1",
		Server:    "github",
		Tool:      "create_issue",
		ArgsHash:  "3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea",
		Decision:  DecisionAllowed,
		DurMs:     42,
		RequestID: "req-1",
	}
	const want = `{"ts":"2026-07-26T12:00:00.123456789Z",` +
		`"actor":"client","client":"claude-code","session":"sess-1",` +
		`"server":"github","tool":"create_issue",` +
		`"argsHash":"3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea",` +
		`"decision":"allowed","durMs":42,"requestID":"req-1"}`
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("golden mismatch:\n got %s\nwant %s", got, want)
	}

	// The zero record still serializes every field (stable shape).
	zero, err := json.Marshal(Record{})
	if err != nil {
		t.Fatal(err)
	}
	const wantZero = `{"ts":"0001-01-01T00:00:00Z","actor":"","client":"",` +
		`"session":"","server":"","tool":"","argsHash":"","decision":"",` +
		`"durMs":0,"requestID":""}`
	if string(zero) != wantZero {
		t.Errorf("zero-record golden mismatch:\n got %s\nwant %s", zero, wantZero)
	}
}

// TestAuditRecordFieldSet locks the field set: the Record type must never
// grow a field that could carry call arguments or results. Adding any
// field is a deliberate act that must update this list (and justify why
// it is not payload).
func TestAuditRecordFieldSet(t *testing.T) {
	want := []string{
		"TS", "Actor", "Client", "Session", "Server", "Tool",
		"ArgsHash", "Decision", "DurMs", "RequestID",
	}
	rt := reflect.TypeOf(Record{})
	var got []string
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Record fields = %v, want exactly %v — audit records must never carry args or results", got, want)
	}
}

func TestAuditStreamEndToEnd(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s, err := NewAuditStream(filepath.Join(dir, AuditFileName), WriterOptions{Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	// Zero TS is filled from the stream clock; non-UTC TS is normalized.
	s.Append(Record{Actor: "client", Decision: DecisionHeld})
	loc := time.FixedZone("UTC+8", 8*3600)
	s.Append(Record{TS: time.Date(2026, 7, 26, 20, 0, 1, 0, loc), Actor: "client", Decision: DecisionAllowed})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, AuditFileName))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var r0, r1 Record
	if err := json.Unmarshal(lines[0], &r0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &r1); err != nil {
		t.Fatal(err)
	}
	if !r0.TS.Equal(clk.now) {
		t.Errorf("zero TS filled as %v, want %v", r0.TS, clk.now)
	}
	if want := time.Date(2026, 7, 26, 12, 0, 1, 0, time.UTC); !r1.TS.Equal(want) || r1.TS.Location() != time.UTC {
		t.Errorf("TS not normalized to UTC: %v", r1.TS)
	}
}
