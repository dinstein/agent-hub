package calllog_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
)

func metadataOnlyStore(t *testing.T) (*calllog.Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "calls")
	s, err := calllog.Open(calllog.Options{Root: root})
	if err != nil {
		t.Fatalf("Open without a key: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

// The default state of an installation: no key, no evidence, and a data plane
// that is nevertheless observable. Before this, a hub with the ledger switched
// off recorded nothing at all about what clients called.
func TestStoreWithoutAKeyRecordsMetadataAndRefusesPayloads(t *testing.T) {
	s, root := metadataOnlyStore(t)

	if s.HasKey() {
		t.Fatal("a keyless store claims it can seal payloads")
	}
	if _, err := s.PutPayload(time.Now(), "call-1", calllog.PayloadRequest, []byte("{}")); err == nil {
		t.Fatal("a keyless store accepted a payload")
	}
	if err := s.Append(calllog.Event{
		Kind: calllog.EventReceived, CallID: "call-1", Client: "claude-code",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, _, err := calllog.ReadEvents(root)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 1 || events[0].CallID != "call-1" {
		t.Fatalf("events = %+v", events)
	}
	// No key means no authentication tag, and saying so is the point: an
	// empty MAC is "nobody claimed this", which a verifier must not report as
	// "somebody's claim failed".
	if events[0].MAC != "" || events[0].KeyID != "" {
		t.Errorf("unkeyed event carries an authentication tag: %+v", events[0])
	}
}

// Frames join the call that caused them. This is the whole reason the trace
// moved in here: the per-server log had no call id, so "this call retried
// twice" was unanswerable from either stream.
func TestFramesJoinTheirCallAndCarryTheirCause(t *testing.T) {
	s, root := metadataOnlyStore(t)

	s.AppendFrame(calllog.Event{
		Kind: calllog.EventSent, CallID: "call-1", Server: "github",
		Method: "tools/call", Cause: calllog.CauseCall, Seq: 1,
	}, []byte(`{"name":"search"}`), false)
	s.AppendFrame(calllog.Event{
		Kind: calllog.EventRecv, CallID: "call-1", Server: "github",
		Method: "tools/call", Cause: calllog.CauseCall, Seq: 1, DurationMs: 12,
	}, []byte(`{"ok":true}`), false)
	// A frame with no call behind it is not an exception: a probe is traffic
	// somebody reading this server's conversation has to account for.
	s.AppendFrame(calllog.Event{
		Kind: calllog.EventSent, Server: "github", Method: "ping", Cause: calllog.CauseProbe,
	}, nil, false)
	s.Sync()

	var frames []calllog.Event
	if _, err := calllog.ScanFramesSince(root, time.Time{}, func(e calllog.Event) error {
		frames = append(frames, e)
		return nil
	}); err != nil {
		t.Fatalf("ScanFramesSince: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if frames[0].CallID != "call-1" || frames[0].Seq != 1 || frames[0].Cause != calllog.CauseCall {
		t.Errorf("call frame lost its join key: %+v", frames[0])
	}
	if frames[2].CallID != "" || frames[2].Cause != calllog.CauseProbe {
		t.Errorf("probe frame = %+v, want no call id and cause=probe", frames[2])
	}
	// The body is not stored without a key, but its SIZE is: a reader can
	// still tell a large frame from a missing one.
	if frames[0].Bytes != len(`{"name":"search"}`) || frames[0].Frame != nil {
		t.Errorf("keyless frame = %+v, want bytes recorded and no payload ref", frames[0])
	}
}

// The lifecycle scan must not silently grow by two orders of magnitude
// because somebody turned tracing on for one server.
func TestLifecycleScanDoesNotReturnFrames(t *testing.T) {
	s, root := metadataOnlyStore(t)

	if err := s.Append(calllog.Event{Kind: calllog.EventReceived, CallID: "call-1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for range 5 {
		s.AppendFrame(calllog.Event{
			Kind: calllog.EventSent, CallID: "call-1", Cause: calllog.CauseCall,
		}, nil, false)
	}
	s.Sync()

	events, _, err := calllog.ReadEvents(root)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != calllog.EventReceived {
		t.Fatalf("lifecycle scan returned %d records, want the one: %+v", len(events), events)
	}
}

// Frames never block their caller. A queue that filled up drops and counts,
// because a trace waiting on a disk would be a debugging aid holding up the
// call it is tracing.
func TestFrameOverflowIsCountedNotWaitedOn(t *testing.T) {
	s, _ := metadataOnlyStore(t)

	for range 20000 {
		s.AppendFrame(calllog.Event{Kind: calllog.EventSent, Cause: calllog.CauseProbe}, nil, false)
	}
	s.Sync()

	c := s.FrameCounters()
	if c.Written+c.Dropped != 20000 {
		t.Fatalf("counters = %+v, want every frame either written or dropped", c)
	}
}
