package api

import (
	"bufio"
	"bytes"
	"io"
)

// sseParser incrementally parses a text/event-stream body (WHATWG SSE
// wire format). It handles arbitrary read fragmentation (the underlying
// bufio.Reader reassembles lines), CRLF/LF line endings, comment lines,
// multi-line data fields and id tracking for Last-Event-ID resumption.
type sseParser struct {
	r *bufio.Reader
	// lastID is the last-seen `id:` value. Per spec it updates as id
	// fields are parsed — even in blocks that never dispatch — and
	// persists across events; ids containing NUL are ignored.
	lastID string
}

func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{r: bufio.NewReaderSize(r, 32<<10)}
}

// next returns the next dispatched event: its event name ("" when the
// stream did not set one) and the concatenated data payload. Blocks with
// no data field are not dispatched (per spec). Returns io.EOF (or the
// transport error) when the stream ends; a trailing incomplete line is
// discarded, never delivered as a truncated event.
func (p *sseParser) next() (name string, data []byte, err error) {
	var buf []byte
	haveData := false
	for {
		line, err := p.readLine()
		if err != nil {
			return "", nil, err
		}
		if len(line) == 0 { // blank line: dispatch if the block had data
			if haveData {
				return name, buf, nil
			}
			name = ""
			continue
		}
		if line[0] == ':' { // comment (keep-alive)
			continue
		}
		field, value := splitSSEField(line)
		switch field {
		case "data":
			if haveData {
				buf = append(buf, '\n')
			}
			buf = append(buf, value...)
			haveData = true
		case "event":
			name = string(value)
		case "id":
			if !bytes.ContainsRune(value, 0) {
				p.lastID = string(value)
			}
		default:
			// Unknown fields (incl. "retry": client owns its backoff) are
			// ignored per spec.
		}
	}
}

// readLine reads one line, stripping the trailing LF/CRLF. On stream end
// with a partial (unterminated) line, the partial line is discarded and
// the error is returned.
func (p *sseParser) readLine() ([]byte, error) {
	line, err := p.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line, nil
}

// splitSSEField splits "field: value". A line without a colon is a field
// with an empty value; a single space after the colon is stripped.
func splitSSEField(line []byte) (field string, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), nil
	}
	field = string(line[:i])
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
