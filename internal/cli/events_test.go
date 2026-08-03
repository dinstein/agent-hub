package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
)

// The follower's cursor used to be read back out of the RENDERED row, whose
// stamp has second resolution, so it advanced a whole second past records it
// had never printed and a burst inside one second silently lost its tail.
// The cursor is the record's own time.Time now, and this pins it: five
// records one microsecond apart must all be reachable, one at a time.
func TestFollowCursorKeepsSubSecondRecords(t *testing.T) {
	base := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	records := make([]eventlog.Record, 0, 5)
	for i := range 5 {
		records = append(records, eventlog.Record{
			TS:   base.Add(time.Duration(i) * time.Microsecond),
			Kind: eventlog.KindConnected, Scope: eventlog.ScopeServer, Server: "s",
		})
	}
	// Walk the cursor exactly as followEvents does: take the newest of what
	// was just emitted, then ask for everything after it.
	seen := newest(records[:1])
	remaining := records
	for printed := 1; printed < len(records); printed++ {
		remaining = after(remaining, seen)
		if len(remaining) != len(records)-printed {
			t.Fatalf("after record %d the cursor left %d records, want %d — "+
				"a same-second record was skipped", printed, len(remaining), len(records)-printed)
		}
		seen = newest(remaining[:1])
	}
	// And the cursor must still be strict: re-asking with the last timestamp
	// returns nothing rather than reprinting it.
	if got := after(records, newest(records)); len(got) != 0 {
		t.Errorf("the newest record was returned again (%d records); a duplicate "+
			"reads as the state having changed twice", len(got))
	}
}

// writeEvents seeds <data>/logs/events.jsonl with raw lines, which is what
// the reader actually meets: a file written by processes that are not here.
func writeEvents(t *testing.T, dataDir string, lines ...string) {
	t.Helper()
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, eventlog.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The question the class exists for: what is unusual here. A filtered read
// must keep the recovery that ends an outage — dropping it shows the server
// going down and never coming back.
func TestEventsClassFiltersDisruptionsAndKeepsTheirRecovery(t *testing.T) {
	dir := setDataDir(t)
	writeEvents(t, dir,
		`{"ts":"2020-01-02T10:00:00Z","scope":"server","kind":"connected","server":"github","pid":1,"count":3}`,
		`{"ts":"2020-01-02T10:00:01Z","scope":"server","kind":"health_down","server":"github","pid":1}`,
		`{"ts":"2020-01-02T10:00:02Z","scope":"server","kind":"health_up","server":"github","pid":1}`,
		`{"ts":"2020-01-02T10:00:03Z","scope":"gateway","kind":"client_attached","client":"cursor","pid":2}`,
	)

	code, out, errOut := runCLI(t, "", "events", "--class", "disruption")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "health_down") || !strings.Contains(out, "health_up") {
		t.Fatalf("a disruption view lost half of its own episode:\n%s", out)
	}
	if strings.Contains(out, "connected\t") || strings.Contains(out, "client_attached") {
		t.Fatalf("routine events leaked into the disruption view:\n%s", out)
	}
}

func TestEventsRoutineClassExcludesFailures(t *testing.T) {
	dir := setDataDir(t)
	writeEvents(t, dir,
		`{"ts":"2020-01-02T10:00:00Z","scope":"server","kind":"connected","server":"github","pid":1}`,
		`{"ts":"2020-01-02T10:00:01Z","scope":"server","kind":"connect_failed","server":"linear","pid":1}`,
	)

	code, out, errOut := runCLI(t, "", "events", "--class", "routine")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if strings.Contains(out, "connect_failed") {
		t.Fatalf("a failure appeared in the routine view:\n%s", out)
	}
	if !strings.Contains(out, "connected") {
		t.Fatalf("the routine view is missing its own events:\n%s", out)
	}
}

// An unknown class is a usage error, never an empty page: "no events" is the
// same answer as "this has not happened", which is the confusion a closed set
// exists to prevent.
func TestEventsUnknownClassIsAUsageError(t *testing.T) {
	setDataDir(t)

	code, _, errOut := runCLI(t, "", "events", "--class", "weird")

	if code == 0 {
		t.Fatal("an unknown class was accepted")
	}
	if !strings.Contains(errOut, "routine") || !strings.Contains(errOut, "disruption") {
		t.Fatalf("the error does not name the known classes: %s", errOut)
	}
}
