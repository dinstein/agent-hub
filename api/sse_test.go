package api

import (
	"io"
	"strings"
	"testing"
)

// drainSSE collects all dispatched events until stream end.
func drainSSE(t *testing.T, r io.Reader) (events []struct {
	name string
	data string
}, lastID string) {
	t.Helper()
	p := newSSEParser(r)
	for {
		name, data, err := p.next()
		if err == io.EOF {
			return events, p.lastID
		}
		if err != nil {
			t.Fatalf("parser error: %v", err)
		}
		events = append(events, struct {
			name string
			data string
		}{name, string(data)})
	}
}

func TestSSEParserBasics(t *testing.T) {
	in := ": keep-alive comment\n" +
		"event: servers\n" +
		"id: 1\n" +
		"data: {\"topic\":\"servers\"}\n" +
		"\n" +
		"data:no-space-after-colon\n" +
		"\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"event: ignored-no-data\n" +
		"\n" +
		"id: 2\n" +
		"data: after-id\n" +
		"\n"
	events, lastID := drainSSE(t, strings.NewReader(in))
	want := []struct{ name, data string }{
		{"servers", `{"topic":"servers"}`},
		{"", "no-space-after-colon"}, // event name resets after dispatch
		{"", "line1\nline2"},         // multi-line data joined with \n
		{"", "after-id"},             // block without data is not dispatched
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].name != w.name || events[i].data != w.data {
			t.Errorf("event %d: got (%q, %q), want (%q, %q)",
				i, events[i].name, events[i].data, w.name, w.data)
		}
	}
	if lastID != "2" {
		t.Errorf("lastID = %q, want 2 (id fields persist even without dispatch)", lastID)
	}
}

func TestSSEParserCRLFAndBareField(t *testing.T) {
	in := "event: x\r\ndata: crlf\r\n\r\ndata\n\n"
	events, _ := drainSSE(t, strings.NewReader(in))
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].name != "x" || events[0].data != "crlf" {
		t.Errorf("CRLF event mangled: %+v", events[0])
	}
	// A bare "data" line (no colon) is a data field with empty value.
	if events[1].data != "" {
		t.Errorf("bare data field: got %q, want empty", events[1].data)
	}
}

func TestSSEParserIDWithNULIgnored(t *testing.T) {
	in := "id: ok\ndata: a\n\nid: bad\x00id\ndata: b\n\n"
	_, lastID := drainSSE(t, strings.NewReader(in))
	if lastID != "ok" {
		t.Errorf("lastID = %q, want ok (NUL ids must be ignored)", lastID)
	}
}

// fragmentedReader yields at most n bytes per Read to simulate arbitrary
// TCP/UDS fragmentation of the stream.
type fragmentedReader struct {
	s string
	n int
}

func (f *fragmentedReader) Read(p []byte) (int, error) {
	if len(f.s) == 0 {
		return 0, io.EOF
	}
	n := min(f.n, len(f.s))
	n = copy(p[:min(n, len(p))], f.s)
	f.s = f.s[n:]
	return n, nil
}

func TestSSEParserFragmentedInput(t *testing.T) {
	in := "event: servers\nid: 42\ndata: {\"topic\":\"servers\",\"kind\":\"changed\"}\n\n" +
		"data: second\n\n"
	for _, frag := range []int{1, 2, 3, 7} {
		events, lastID := drainSSE(t, &fragmentedReader{s: in, n: frag})
		if len(events) != 2 {
			t.Fatalf("frag=%d: got %d events, want 2", frag, len(events))
		}
		if events[0].data != `{"topic":"servers","kind":"changed"}` || events[0].name != "servers" {
			t.Errorf("frag=%d: first event mangled: %+v", frag, events[0])
		}
		if events[1].data != "second" {
			t.Errorf("frag=%d: second event mangled: %+v", frag, events[1])
		}
		if lastID != "42" {
			t.Errorf("frag=%d: lastID = %q, want 42", frag, lastID)
		}
	}
}

// TestSSEParserTrailingPartialLineDiscarded: an unterminated final line
// must be discarded, never delivered as a truncated event.
func TestSSEParserTrailingPartialLineDiscarded(t *testing.T) {
	in := "data: complete\n\ndata: truncat" // no trailing newline
	events, _ := drainSSE(t, strings.NewReader(in))
	if len(events) != 1 || events[0].data != "complete" {
		t.Fatalf("got %+v, want only the complete event", events)
	}
}
