package transport

import (
	"errors"
	"io"
	"strings"
	"testing"

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
		for i := 0; i < 20; i++ {
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
