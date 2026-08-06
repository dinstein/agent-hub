package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

func TestSSEScannerDispatch(t *testing.T) {
	stream := strings.Join([]string{
		": keep-alive comment",
		"",
		"event: endpoint",
		"data: /messages?sid=abc",
		"",
		"id: 7",
		"data: {\"a\":1}",
		"data: {\"b\":2}",
		"",
		"retry: 500",
		"data: plain",
		"",
	}, "\n") + "\n"

	sc := newSSEScanner(strings.NewReader(stream), mcp.MaxFrameSize)

	ev, err := sc.Next()
	if err != nil {
		t.Fatalf("event 1: %v", err)
	}
	if ev.name != "endpoint" || string(ev.data) != "/messages?sid=abc" {
		t.Fatalf("event 1 = %q / %q", ev.name, ev.data)
	}

	ev, err = sc.Next()
	if err != nil {
		t.Fatalf("event 2: %v", err)
	}
	if ev.name != "message" {
		t.Fatalf("event 2 name = %q, want default message", ev.name)
	}
	if ev.id != "7" || sc.lastEventID() != "7" {
		t.Fatalf("event 2 id = %q / lastEventID %q", ev.id, sc.lastEventID())
	}
	// Multiple data lines join with \n and the trailing newline is stripped.
	if string(ev.data) != "{\"a\":1}\n{\"b\":2}" {
		t.Fatalf("event 2 data = %q", ev.data)
	}

	ev, err = sc.Next()
	if err != nil {
		t.Fatalf("event 3: %v", err)
	}
	if string(ev.data) != "plain" || ev.id != "" {
		t.Fatalf("event 3 = %q id %q", ev.data, ev.id)
	}
	// lastEventID persists across events that carry no id.
	if sc.lastEventID() != "7" {
		t.Fatalf("lastEventID = %q, want sticky 7", sc.lastEventID())
	}

	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v, want EOF", err)
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF must be sticky, got %v", err)
	}
}

func TestSSEScannerCRLFAndIncompleteTail(t *testing.T) {
	// CRLF line endings, and a final event with no terminating blank line
	// (discarded per the SSE dispatch rule).
	stream := "event: message\r\ndata: one\r\n\r\ndata: dangling\r\n"
	sc := newSSEScanner(strings.NewReader(stream), mcp.MaxFrameSize)
	ev, err := sc.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if string(ev.data) != "one" {
		t.Fatalf("data = %q", ev.data)
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("incomplete tail must be discarded, got %v", err)
	}
}

func TestSSEScannerBound(t *testing.T) {
	t.Run("one oversized data line", func(t *testing.T) {
		stream := "data: " + strings.Repeat("x", 200) + "\n\n"
		sc := newSSEScanner(strings.NewReader(stream), 64)
		if _, err := sc.Next(); !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
		if _, err := sc.Next(); !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("bound error must be sticky, got %v", err)
		}
	})
	t.Run("accumulated data lines", func(t *testing.T) {
		var b strings.Builder
		for range 20 {
			b.WriteString("data: " + strings.Repeat("y", 30) + "\n")
		}
		b.WriteString("\n")
		sc := newSSEScanner(strings.NewReader(b.String()), 64)
		if _, err := sc.Next(); !errors.Is(err, mcp.ErrFrameTooLarge) {
			t.Fatalf("err = %v, want ErrFrameTooLarge", err)
		}
	})
	t.Run("under the bound still passes", func(t *testing.T) {
		sc := newSSEScanner(strings.NewReader("data: "+strings.Repeat("z", 40)+"\n\n"), 64)
		ev, err := sc.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ev.data) != 40 {
			t.Fatalf("len(data) = %d", len(ev.data))
		}
	})
}

// TestSSEScannerRetryHint: the one SSE field a client MUST act on rather
// than merely tolerate (MCP 2025-11-25, transports §"Sending Messages to the
// Server" item 6). The scanner parsed it and dropped it on the floor until
// this test existed, and the comment that justified dropping it claimed a
// bounded backoff the live reconnect path did not have.
func TestSSEScannerRetryHint(t *testing.T) {
	t.Run("kept across events", func(t *testing.T) {
		sc := newSSEScanner(strings.NewReader("retry: 30000\ndata: x\n\ndata: y\n\n"), mcp.MaxFrameSize)
		if _, err := sc.Next(); err != nil {
			t.Fatal(err)
		}
		if got := sc.retryHint(); got != 30*time.Second {
			t.Fatalf("retryHint = %v, want 30s", got)
		}
		// It is stream state, not event state: an event carrying no retry
		// does not clear the last one.
		if _, err := sc.Next(); err != nil {
			t.Fatal(err)
		}
		if got := sc.retryHint(); got != 30*time.Second {
			t.Fatalf("retryHint after a retry-less event = %v, want 30s", got)
		}
	})
	t.Run("none asked for", func(t *testing.T) {
		sc := newSSEScanner(strings.NewReader("data: x\n\n"), mcp.MaxFrameSize)
		if _, err := sc.Next(); err != nil {
			t.Fatal(err)
		}
		if got := sc.retryHint(); got != 0 {
			t.Fatalf("retryHint = %v, want 0", got)
		}
	})
	t.Run("invalid values leave the last hint standing", func(t *testing.T) {
		// The SSE standard says a retry that is not ASCII digits is ignored
		// — ignored, not treated as zero, which would be a reconnect storm
		// dressed up as conformance.
		for _, bad := range []string{"", "+5", "-5", " 500", "500ms", "1e3", "99999999999999999999"} {
			sc := newSSEScanner(strings.NewReader("retry: 700\nretry: "+bad+"\ndata: x\n\n"), mcp.MaxFrameSize)
			if _, err := sc.Next(); err != nil {
				t.Fatal(err)
			}
			if got := sc.retryHint(); got != 700*time.Millisecond {
				t.Fatalf("retry %q: hint = %v, want the previous 700ms to stand", bad, got)
			}
		}
	})
	t.Run("zero is a valid ask", func(t *testing.T) {
		sc := newSSEScanner(strings.NewReader("retry: 900\nretry: 0\ndata: x\n\n"), mcp.MaxFrameSize)
		if _, err := sc.Next(); err != nil {
			t.Fatal(err)
		}
		if got := sc.retryHint(); got != 0 {
			t.Fatalf("retryHint = %v, want an explicit 0 to replace the 900ms", got)
		}
	})
}

// TestWaitBeforeResume: the client's half of the retry MUST. It cannot always
// wait — the per-call resume runs under the caller's deadline — so the rule
// it keeps is the narrower one the MUST protects: never come back sooner
// than asked. Not resuming at all is the fallback; resuming early is not.
func TestWaitBeforeResume(t *testing.T) {
	t.Run("no hint resumes at once", func(t *testing.T) {
		start := time.Now()
		if !waitBeforeResume(context.Background(), 0) {
			t.Fatal("no hint must not block a resume")
		}
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("waited %v with no hint", d)
		}
	})
	t.Run("a hint that fits is waited out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if !waitBeforeResume(ctx, 120*time.Millisecond) {
			t.Fatal("a hint inside the deadline must still allow the resume")
		}
		if d := time.Since(start); d < 120*time.Millisecond {
			t.Fatalf("came back after %v, sooner than the 120ms asked for", d)
		}
	})
	t.Run("a hint longer than the deadline abandons the resume", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		if waitBeforeResume(ctx, time.Hour) {
			t.Fatal("resumed despite being unable to wait as long as asked")
		}
		// And it refuses immediately rather than burning the deadline first.
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("took %v to decide it could not wait", d)
		}
	})
	t.Run("cancellation ends the wait without resuming", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		if waitBeforeResume(ctx, time.Minute) {
			t.Fatal("a cancelled wait must not resume")
		}
	})
}

func TestSSEScannerIgnoresNULIDAndUnknownFields(t *testing.T) {
	stream := "id: bad\x00id\nfoo: bar\ndata: x\n\n"
	sc := newSSEScanner(strings.NewReader(stream), mcp.MaxFrameSize)
	ev, err := sc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.id != "" || sc.lastEventID() != "" {
		t.Fatalf("NUL-bearing id must be ignored, got %q", ev.id)
	}
	if string(ev.data) != "x" {
		t.Fatalf("data = %q", ev.data)
	}
}
