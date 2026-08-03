package ctlapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/eventlog"
)

// seedEventLog writes a small stream and returns its path.
func seedEventLog(t *testing.T, records ...eventlog.Record) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), eventlog.FileName)
	s, err := eventlog.Open(path, eventlog.Options{PID: 77})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		s.Append(r)
	}
	s.Sync()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The route exists so the GUI can read a file it may not import (AGENTS.md
// hard constraint 1). What it must deliver is the record intact, including
// the identity fields that make a shared file placeable.
func TestEventLogServesRecordsAndFilters(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	path := seedEventLog(t,
		eventlog.Record{TS: base, Scope: eventlog.ScopeDaemon, Kind: eventlog.KindDaemonStarted},
		eventlog.Record{
			TS: base.Add(time.Second), Scope: eventlog.ScopeServer, Kind: eventlog.KindConnected,
			Server: "github", Client: "claude-code",
		},
		eventlog.Record{
			TS: base.Add(2 * time.Second), Scope: eventlog.ScopeServer, Kind: eventlog.KindCircuitOpen,
			Server: "github", Client: "claude-code", From: "closed", To: "open", DurMs: 20000,
		},
		eventlog.Record{
			TS: base.Add(3 * time.Second), Scope: eventlog.ScopeServer, Kind: eventlog.KindConnected,
			Server: "linear", Client: "cursor",
		},
	)
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.EventLogPath = path })

	all, err := client.Events.Log(t.Context(), api.EventLogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 4 || all.Files != 1 {
		t.Fatalf("unfiltered = %d events over %d files", len(all.Events), all.Files)
	}
	// Oldest first, so a UI can append rather than re-sort.
	if !all.Events[0].TS.Equal(base) {
		t.Errorf("first record = %v, want the oldest", all.Events[0].TS)
	}
	circuit := all.Events[2]
	if circuit.Server != "github" || circuit.Client != "claude-code" || circuit.PID != 77 {
		t.Errorf("identity lost in transit: %+v", circuit)
	}
	if circuit.From != "closed" || circuit.To != "open" || circuit.DurMs != 20000 {
		t.Errorf("transition lost in transit: %+v", circuit)
	}

	byServer, err := client.Events.Log(t.Context(), api.EventLogFilter{Server: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byServer.Events) != 2 {
		t.Errorf("--server github = %d events", len(byServer.Events))
	}
	byKind, err := client.Events.Log(t.Context(), api.EventLogFilter{
		Scope: string(eventlog.ScopeServer), Kinds: []string{string(eventlog.KindCircuitOpen)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind.Events) != 1 || byKind.Events[0].Kind != string(eventlog.KindCircuitOpen) {
		t.Errorf("kind filter = %+v", byKind.Events)
	}
	// The tail, not the head: an incident is at the end of the stream.
	tail, err := client.Events.Log(t.Context(), api.EventLogFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Events) != 1 || tail.Events[0].Server != "linear" {
		t.Errorf("limit 1 = %+v, want the newest record", tail.Events)
	}
}

// A closed vocabulary exists so a caller can be TOLD it got a name wrong.
// Answering with an empty page would be the same response as "this has not
// happened", which is the confusion the closed set prevents.
func TestEventLogRejectsUnknownSelectors(t *testing.T) {
	path := seedEventLog(t, eventlog.Record{Scope: eventlog.ScopeDaemon, Kind: eventlog.KindDaemonStarted})
	client, _ := startServer(t, func(o *Options) { o.NonRegistry.EventLogPath = path })

	for _, f := range []api.EventLogFilter{
		{Scope: "nonsense"},
		{Kinds: []string{"exploded"}},
		// Real at another scope is still wrong at this one.
		{Scope: string(eventlog.ScopeServer), Kinds: []string{string(eventlog.KindListenerBound)}},
		{Limit: eventLogMaxLimit + 1},
	} {
		if _, err := client.Events.Log(t.Context(), f); err == nil {
			t.Errorf("filter %+v was accepted", f)
		}
	}
}

// An unassembled daemon answers the uniform 404 rather than pretending the
// stream is empty — the same rule every other optional surface follows.
func TestEventLogAbsentWithoutAPath(t *testing.T) {
	client, _ := startServer(t, func(o *Options) {})
	if _, err := client.Events.Log(t.Context(), api.EventLogFilter{}); err == nil {
		t.Fatal("the route answered without a configured path")
	}
}
