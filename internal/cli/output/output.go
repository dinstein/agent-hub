// Package output is the single rendering layer for every agenthub command:
// the human-readable path and the --json path are fed by the same data value,
// so the two representations can never drift semantically (docs/modules/controlplane.md
// rule 2: "every command has --json; human and machine output are rendered
// from the same data structure").
//
// JSON envelope (docs/modules/controlplane.md):
//
//	{"ok":true,"data":...,"warnings":[...]}
//	{"ok":false,"error":{"code":...,"message":...,"hint":...}}
//
// Invariants:
//   - Emit accepts exactly one Data value. The JSON path marshals that value
//     verbatim as the envelope's "data"; the human path calls its Human
//     method. There is no second code path that could render different
//     content for the two modes.
//   - In JSON mode the whole envelope goes to the primary writer (stdout) as
//     a single line so scripts can parse output line-by-line. In human mode
//     warnings and errors go to the secondary writer (stderr), keeping
//     stdout clean for tables and snippets.
//   - The success envelope always carries "data" and "warnings" keys (the
//     warnings array is never null); the failure envelope always carries
//     "error" with at least "code" and "message".
package output

import (
	"encoding/json"
	"fmt"
	"io"
)

// Data is a command result that can render itself for humans. The same value
// is marshaled as-is on the --json path, which is what guarantees the two
// output modes share a single source.
type Data interface {
	// Human writes the human-readable rendering to w.
	Human(w io.Writer) error
}

// ErrorDetail is the "error" object of the failure envelope.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// okEnvelope is the success envelope. Warnings is never nil on the wire.
type okEnvelope struct {
	OK       bool     `json:"ok"`
	Data     Data     `json:"data"`
	Warnings []string `json:"warnings"`
}

// errEnvelope is the failure envelope.
type errEnvelope struct {
	OK    bool        `json:"ok"`
	Error ErrorDetail `json:"error"`
}

// Printer renders command results in exactly one of the two modes.
// A Printer is stateless apart from its writers and is safe to reuse for
// several Emit/Fail calls within one command invocation.
type Printer struct {
	out      io.Writer // stdout: results (and, in JSON mode, everything)
	errOut   io.Writer // stderr: human-mode warnings and errors
	jsonMode bool
}

// New returns a Printer writing results to out and (in human mode)
// warnings/errors to errOut.
func New(out, errOut io.Writer, jsonMode bool) *Printer {
	return &Printer{out: out, errOut: errOut, jsonMode: jsonMode}
}

// JSONMode reports whether the printer renders the JSON envelope.
func (p *Printer) JSONMode() bool { return p.jsonMode }

// Emit renders one successful result from a single data value.
func (p *Printer) Emit(data Data, warnings ...string) error {
	if p.jsonMode {
		if warnings == nil {
			warnings = []string{}
		}
		return encodeLine(p.out, okEnvelope{OK: true, Data: data, Warnings: warnings})
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(p.errOut, "warning: %s\n", w)
	}
	return data.Human(p.out)
}

// ProgressEvent is one intermediate step of a long-running command. Four
// stream today — auth login, server test, server enable's probe, and doctor
// — and docs/modules/controlplane.md carries the same list next to the
// parsing rule it decides. Adding a fifth means updating that list and the
// shipped skill's, because both tell a consumer whether to parse one JSON
// object or a stream of them.
//
// Rendering rules, both deliberate:
//
//   - JSON mode: one compact line {"event":"awaiting_browser",...} on
//     stdout, so a script consuming the stream line-by-line sees progress
//     before the final envelope, which is always the LAST line.
//   - Human mode: the message goes to STDERR. Progress is not the result;
//     keeping stdout to the result alone means `agenthub auth status | jq`
//     and shell pipelines behave the same in both modes.
//
// Fields carry structured detail (verification_uri, interval, ...). A field
// named "event" is ignored — the event name has one source.
type ProgressEvent struct {
	// Event is the stable machine name of the step (snake_case).
	Event string
	// Message is the human rendering. Empty falls back to Event.
	Message string
	// Fields are extra JSON members of the event line.
	Fields map[string]any
}

// MarshalJSON renders the event as a flat object with "event" first.
func (e ProgressEvent) MarshalJSON() ([]byte, error) {
	obj := make(map[string]any, len(e.Fields)+1)
	for k, v := range e.Fields {
		if k == "event" {
			continue
		}
		obj[k] = v
	}
	obj["event"] = e.Event
	return json.Marshal(obj)
}

// Progress emits one progress line. It never returns an error: a command
// whose progress line fails to write must still be able to finish and
// report its actual result.
func (p *Printer) Progress(ev ProgressEvent) {
	if p.jsonMode {
		_ = encodeLine(p.out, ev)
		return
	}
	msg := ev.Message
	if msg == "" {
		msg = ev.Event
	}
	_, _ = fmt.Fprintln(p.errOut, msg)
}

// Fail renders one error. It never returns an error itself: failing to
// report a failure has no better recourse than best effort.
func (p *Printer) Fail(d ErrorDetail) {
	if p.jsonMode {
		_ = encodeLine(p.out, errEnvelope{OK: false, Error: d})
		return
	}
	_, _ = fmt.Fprintf(p.errOut, "agenthub: %s\n", d.Message)
	if d.Hint != "" {
		_, _ = fmt.Fprintf(p.errOut, "hint: %s\n", d.Hint)
	}
}

// encodeLine writes v as one compact JSON line.
func encodeLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
