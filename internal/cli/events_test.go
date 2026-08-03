package cli

import (
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
