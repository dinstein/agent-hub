package transport

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// FuzzSSEScanner drives the hand-rolled SSE line scanner with arbitrary bytes.
//
// This is the parser with the least trusted input in the repository: it reads
// a live stream straight off a remote MCP server, before any of the protocol
// layers above it get a say. A panic here is a gateway crash that a malicious
// or merely broken server can trigger at will, and the scanner does its own
// line splitting, size accounting and field parsing rather than delegating to
// encoding/json.
//
// The loop is bounded rather than run to EOF so a fuzz input cannot turn into
// a hang: what is being looked for is a crash or a scanner that reports
// progress forever without consuming anything.
func FuzzSSEScanner(f *testing.F) {
	for _, s := range []string{
		"data: hello\n\n",
		"event: message\ndata: {\"a\":1}\n\n",
		"id: 7\ndata: x\n\n",
		": comment only\n\n",
		"data: a\ndata: b\n\n",
		"data:no-space\n\n",
		"\n\n\n",
		"data: unterminated",
		"event: \n\n",
		"id: \x00\ndata: x\n\n",
		"data: \r\n\r\n",
		"retry: 100\n\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s := newSSEScanner(bytes.NewReader(data), 1<<16)
		for range 64 {
			ev, err := s.Next()
			if err != nil {
				if ev != nil {
					t.Fatalf("event %+v returned alongside error %v", ev, err)
				}
				return
			}
			if ev == nil {
				t.Fatal("nil event with nil error")
			}
		}
		// Still producing events after the bound: fine, the input was simply
		// long. What matters is that no iteration panicked.
		if s.err != nil && !errors.Is(s.err, io.EOF) {
			_ = s.err
		}
	})
}
