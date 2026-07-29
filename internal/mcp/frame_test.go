package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameReaderNext(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		max   int
		want  []string
		final error // error after the listed frames
	}{
		{
			name:  "single frame",
			in:    "{\"a\":1}\n",
			max:   64,
			want:  []string{`{"a":1}`},
			final: io.EOF,
		},
		{
			name:  "multiple frames",
			in:    "{\"a\":1}\n{\"b\":2}\n",
			max:   64,
			want:  []string{`{"a":1}`, `{"b":2}`},
			final: io.EOF,
		},
		{
			name:  "blank lines skipped",
			in:    "\n\n{\"a\":1}\n   \n{\"b\":2}\n\n",
			max:   64,
			want:  []string{`{"a":1}`, `{"b":2}`},
			final: io.EOF,
		},
		{
			name:  "crlf stripped",
			in:    "{\"a\":1}\r\n",
			max:   64,
			want:  []string{`{"a":1}`},
			final: io.EOF,
		},
		{
			name:  "final frame without newline",
			in:    "{\"a\":1}\n{\"b\":2}",
			max:   64,
			want:  []string{`{"a":1}`, `{"b":2}`},
			final: io.EOF,
		},
		{
			name:  "oversized frame",
			in:    strings.Repeat("x", 65) + "\n",
			max:   64,
			want:  nil,
			final: ErrFrameTooLarge,
		},
		{
			name:  "frame exactly at limit passes",
			in:    strings.Repeat("x", 64) + "\n",
			max:   64,
			want:  []string{strings.Repeat("x", 64)},
			final: io.EOF,
		},
		{
			name:  "oversized without trailing newline",
			in:    strings.Repeat("x", 100),
			max:   64,
			want:  nil,
			final: ErrFrameTooLarge,
		},
		{
			name:  "good frame then oversized",
			in:    "{\"a\":1}\n" + strings.Repeat("x", 65) + "\n{\"b\":2}\n",
			max:   64,
			want:  []string{`{"a":1}`},
			final: ErrFrameTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := NewFrameReaderSize(strings.NewReader(tt.in), tt.max)
			for i, want := range tt.want {
				got, err := fr.Next()
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if string(got) != want {
					t.Fatalf("frame %d = %q, want %q", i, got, want)
				}
			}
			_, err := fr.Next()
			if !errors.Is(err, tt.final) {
				t.Fatalf("final error = %v, want %v", err, tt.final)
			}
			// Errors are sticky: the reader is poisoned for good.
			_, err2 := fr.Next()
			if !errors.Is(err2, tt.final) {
				t.Fatalf("sticky error = %v, want %v", err2, tt.final)
			}
		})
	}
}

// TestFrameReaderRealLimit hits the real 16 MiB bound: MaxFrameSize bytes
// pass, MaxFrameSize+1 poison the reader.
func TestFrameReaderRealLimit(t *testing.T) {
	okFrame := bytes.Repeat([]byte("a"), MaxFrameSize)
	fr := NewFrameReader(io.MultiReader(bytes.NewReader(okFrame), strings.NewReader("\n")))
	got, err := fr.Next()
	if err != nil {
		t.Fatalf("frame at exactly MaxFrameSize: %v", err)
	}
	if len(got) != MaxFrameSize {
		t.Fatalf("len = %d", len(got))
	}

	tooBig := bytes.Repeat([]byte("a"), MaxFrameSize+1)
	fr = NewFrameReader(io.MultiReader(bytes.NewReader(tooBig), strings.NewReader("\n")))
	_, err = fr.Next()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := fr.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("reader not poisoned: %v", err)
	}
}

func TestFrameWriterFraming(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	if err := fw.WriteFrame(NewRequest(NewIntID(1), "ping", nil)); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteFrame(NewNotification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	for _, l := range lines {
		if _, err := ParseMessage([]byte(l)); err != nil {
			t.Fatalf("line %q does not parse: %v", l, err)
		}
	}
}

func TestFrameWriterEscapesNewlines(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	params, err := json.Marshal(map[string]string{"text": "line1\nline2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteFrame(NewNotification("x", params)); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(buf.Bytes(), []byte("\n")); got != 1 {
		t.Fatalf("frame contains %d raw newlines, want exactly the terminator", got)
	}
}

func TestFrameWriterTooLarge(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	huge := NewNotification("x", bytes.Repeat([]byte("1"), MaxFrameSize+1))
	err := fw.WriteFrame(huge)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("oversized frame leaked %d bytes onto the wire", buf.Len())
	}
	// The writer stays usable: nothing was written.
	if err := fw.WriteFrame(NewNotification("y", nil)); err != nil {
		t.Fatal(err)
	}
}
