package downstream

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
)

// captureSink records the last frame event handed to it.
type captureSink struct{ last calllog.Event }

func (c *captureSink) AppendFrame(e calllog.Event, _ []byte, _ bool) { c.last = e }

// TestFrameRecvScrubsErrorDetail is the regression for the 2026-08-10 sweep's
// finding that a downstream error body reached the plaintext frame ledger. The
// error string a failed recv records can quote an HTTP body snippet or a peer
// JSON-RPC message carrying a credential, and the frame metadata line is
// written even with capture off. It must be scrubbed before it is persisted.
func TestFrameRecvScrubsErrorDetail(t *testing.T) {
	sink := &captureSink{}
	l := NewFrameLog(sink, "srv", "inst", true /*enabled*/, false /*capture*/)

	const secret = "ghp_0123456789abcdefghij01"
	l.recv(CallOrigin("c1"), 1, "tools/call", nil,
		errors.New("downstream 500: body sent Bearer "+secret), 3*time.Millisecond)

	if sink.last.Error == "" {
		t.Fatal("no error recorded")
	}
	if strings.Contains(sink.last.Error, secret) {
		t.Fatalf("credential persisted verbatim in the frame ledger: %q", sink.last.Error)
	}
	if sink.last.Outcome != "error" {
		t.Errorf("outcome = %q, want error", sink.last.Outcome)
	}
}
