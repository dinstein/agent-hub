package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameSize is the hard upper bound on a single JSON-RPC frame
// (newline excluded), applied on both read and write. 16 MiB, frozen by
// (bounded read is a protocol-layer invariant of this facade).
const MaxFrameSize = 16 << 20

// ErrFrameTooLarge is the decidable sentinel for a frame exceeding
// MaxFrameSize. Once a FrameReader hits it the reader is poisoned: the
// stream position is undefined mid-frame, so the connection must be
// considered unusable (every subsequent Next returns the same error).
var ErrFrameTooLarge = errors.New("jsonrpc frame exceeds size limit")

// FrameReader reads newline-delimited JSON frames (the MCP stdio framing;
// LSP-style Content-Length headers are deliberately not supported).
//
// Properties:
//   - bounded: at most max+bufio-buffer bytes of a frame are ever held;
//     an oversized frame fails early, before being buffered whole,
//   - blank lines (including CRLF artifacts) are skipped,
//   - a trailing frame without a final newline is still delivered,
//   - all errors are sticky: after any error the reader stays failed.
//
// FrameReader is not safe for concurrent use; the transport read loop is
// its single consumer.
type FrameReader struct {
	br  *bufio.Reader
	max int
	err error
}

// NewFrameReader returns a FrameReader with the MaxFrameSize bound.
func NewFrameReader(r io.Reader) *FrameReader {
	return NewFrameReaderSize(r, MaxFrameSize)
}

// NewFrameReaderSize returns a FrameReader with a custom bound (tests and
// fault-injection harnesses only; production code uses NewFrameReader).
// max must be > 0.
func NewFrameReaderSize(r io.Reader, max int) *FrameReader {
	return &FrameReader{br: bufio.NewReader(r), max: max}
}

// Next returns the next non-blank frame with the trailing newline (and any
// carriage return) stripped. It returns io.EOF at clean end of stream,
// ErrFrameTooLarge (wrapped) on an oversized frame, and the underlying read
// error otherwise. The returned slice is owned by the caller.
func (fr *FrameReader) Next() ([]byte, error) {
	if fr.err != nil {
		return nil, fr.err
	}
	for {
		line, err := fr.readLine()
		if err != nil {
			fr.err = err
			return nil, err
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			continue // skip blank lines
		}
		return line, nil
	}
}

// readLine accumulates one '\n'-terminated line, enforcing the size bound
// incrementally so an oversized frame fails without being fully buffered.
// At EOF a non-empty partial line is delivered as a final frame and the
// EOF is made sticky for the next call.
func (fr *FrameReader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := fr.br.ReadSlice('\n')
		buf = append(buf, chunk...)
		n := len(buf)
		if n > 0 && buf[n-1] == '\n' {
			n--
		}
		if n > fr.max {
			return nil, fmt.Errorf("%w: frame larger than %d bytes", ErrFrameTooLarge, fr.max)
		}
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil // strip '\n'
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(bytes.TrimSpace(buf)) > 0:
			// Deliver the unterminated final frame now; EOF next time.
			fr.err = io.EOF
			return buf, nil
		default:
			return nil, err
		}
	}
}

// FrameWriter writes newline-delimited JSON frames. It is safe for
// concurrent use: the transport's Call/Notify goroutines and the read loop
// (peer-request replies) share one writer, and each frame is written as a
// single Write call so frames never interleave.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter returns a FrameWriter over w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// WriteFrame marshals msg and writes it as one newline-terminated frame.
// Frames over MaxFrameSize are rejected with ErrFrameTooLarge before any
// byte is written, so an oversized outgoing frame never corrupts the stream
// and the connection stays usable.
func (fw *FrameWriter) WriteFrame(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if len(data) > MaxFrameSize {
		return fmt.Errorf("%w: outgoing frame is %d bytes", ErrFrameTooLarge, len(data))
	}
	// json.Marshal escapes control characters inside strings, so data
	// contains no raw newline; appending '\n' preserves framing.
	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, data...)
	buf = append(buf, '\n')

	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(buf); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}
