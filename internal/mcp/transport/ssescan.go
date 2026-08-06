package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// sseEvent is one dispatched Server-Sent Event.
type sseEvent struct {
	// id is the event's last id, "" when the server sent none. It feeds
	// Last-Event-ID on resumption.
	id string
	// name is the event type; "message" when the server sent none.
	name string
	// data is the concatenated data lines with the trailing newline
	// stripped (the SSE dispatch rule).
	data []byte
}

// sseScanner parses a text/event-stream body into events.
//
// Properties (mirroring mcp.FrameReader, which is the stdio analogue):
//   - bounded: the accumulated data of one event never exceeds max; an
//     oversized event fails with mcp.ErrFrameTooLarge instead of being
//     buffered whole,
//   - sticky: after any error the scanner stays failed,
//   - an incomplete event at EOF is discarded, per the SSE dispatch rule,
//   - comment lines (":" prefix) and unknown fields are ignored, never
//     fatal — a heartbeat comment must not kill a stream.
//
// ONE DELIBERATE DEVIATION from the dispatch rule, and every caller depends on
// knowing it: the spec drops an event whose data buffer is empty, and this
// scanner dispatches it. A bare `id: 5` followed by a blank line — the shape a
// resumable stream uses to advance Last-Event-ID without carrying a message —
// arrives here as an sseEvent named "message" with no data. It works this way
// because lastID must advance for resumption whether or not anything is
// dispatched, and one rule that always dispatches is easier to hold in the
// head than two paths that both have to remember to update it. THE COST IS
// PAID BY THE CALLER: all three consumers (httpsse.go, and streamablehttp.go
// twice) skip an empty-data event before parsing, and deleting one of those
// guards hands ParseMessage an empty frame — a malformed-frame error, which
// takes the stream down, on a keep-alive.
//
// It is not safe for concurrent use; one stream reader owns it.
type sseScanner struct {
	br         *bufio.Reader
	max        int
	err        error
	pendingEOF bool
	lastID     string
	retry      time.Duration
}

func newSSEScanner(r io.Reader, max int) *sseScanner {
	return &sseScanner{br: bufio.NewReader(r), max: max}
}

// lastEventID returns the most recent id field seen, for Last-Event-ID
// resumption.
func (s *sseScanner) lastEventID() string { return s.lastID }

// retryHint returns the reconnection delay the server last asked for, or 0
// when it asked for none.
//
// MCP 2025-11-25 (basic/transports.mdx, "Sending Messages to the Server"
// item 6) makes this the one SSE field a client is not free to read as
// advice: a server closing the connection without terminating the stream
// SHOULD send `retry`, and "the client MUST respect the retry field, waiting
// the given number of milliseconds before attempting to reconnect". No other
// revision carries the rule — 2025-06-18 and earlier draw no
// connection-versus-stream distinction, and 2026-07-28 removed resumable
// streams outright — but 2025-11-25 is the revision mcp.ProtocolVersion
// names, so it is not a dead letter.
//
// The value is taken only when it is what the SSE standard calls valid:
// ASCII digits and nothing else. Anything else, an overflow included, leaves
// the previous hint standing, which is that standard's own rule for a retry
// it cannot parse.
func (s *sseScanner) retryHint() time.Duration { return s.retry }

// Next returns the next dispatched event, io.EOF at end of stream, or the
// underlying read error. Errors are sticky.
func (s *sseScanner) Next() (*sseEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	var (
		name string
		data []byte
		id   string
		seen bool
	)
	for {
		line, err := s.readLine()
		if err != nil {
			s.err = err
			return nil, err
		}
		if len(line) == 0 {
			if !seen {
				continue // stray blank line: nothing to dispatch
			}
			data = bytes.TrimSuffix(data, []byte("\n"))
			if name == "" {
				name = "message"
			}
			return &sseEvent{id: id, name: name, data: data}, nil
		}
		if line[0] == ':' {
			continue // comment / heartbeat
		}
		field, value, found := strings.Cut(string(line), ":")
		if !found {
			field, value = string(line), ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name, seen = value, true
		case "data":
			if len(data)+len(value)+1 > s.max {
				s.err = fmt.Errorf("%w: sse event data larger than %d bytes", mcp.ErrFrameTooLarge, s.max)
				return nil, s.err
			}
			data = append(data, value...)
			data = append(data, '\n')
			seen = true
		case "id":
			// The SSE spec says an id containing NUL is ignored.
			if !strings.ContainsRune(value, 0) {
				id, s.lastID, seen = value, value, true
			}
		case "retry":
			// The one field this scanner keeps for the caller rather than
			// the stream: see retryHint. Digits only, per the SSE standard,
			// and an unparseable value leaves the last hint standing rather
			// than clearing it.
			if d, ok := parseRetryMS(value); ok {
				s.retry = d
			}
			seen = true
		default:
			// Unknown field: ignored per spec.
		}
	}
}

// parseRetryMS reads an SSE retry value: ASCII digits only, so a sign, a
// unit suffix or any whitespace makes it invalid rather than tolerated
// (strconv would accept a leading "+"). An empty value, and one whose
// milliseconds do not fit a time.Duration, are invalid too — the standard
// says an unparseable retry is ignored, and "ignored" must not become an
// overflowed negative delay.
func parseRetryMS(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms > math.MaxInt64/int64(time.Millisecond) {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// readLine returns one line with the terminator stripped. It enforces the
// size bound incrementally, so a server streaming one endless line fails
// early rather than exhausting memory. A final unterminated line is
// delivered, with EOF made sticky for the next call.
func (s *sseScanner) readLine() ([]byte, error) {
	if s.pendingEOF {
		return nil, io.EOF
	}
	var buf []byte
	for {
		chunk, err := s.br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > s.max+1 {
			return nil, fmt.Errorf("%w: sse line larger than %d bytes", mcp.ErrFrameTooLarge, s.max)
		}
		switch {
		case err == nil:
			return bytes.TrimSuffix(buf[:len(buf)-1], []byte("\r")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			s.pendingEOF = true
			return bytes.TrimSuffix(buf, []byte("\r")), nil
		default:
			return nil, err
		}
	}
}
