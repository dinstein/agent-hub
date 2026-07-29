package output_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/cli/output"
)

// sample is a command result: one value feeding both output modes. The test
// below proves the two renderings agree because Emit accepts only this
// single value — there is no second data path to drift.
type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (s sample) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "name=%s count=%d\n", s.Name, s.Count)
	return err
}

func TestEmitHumanAndJSONShareOneSource(t *testing.T) {
	data := sample{Name: "github", Count: 26}

	var jsonBuf, jsonErr bytes.Buffer
	if err := output.New(&jsonBuf, &jsonErr, true).Emit(data); err != nil {
		t.Fatalf("json Emit: %v", err)
	}
	var env struct {
		OK       bool     `json:"ok"`
		Data     sample   `json:"data"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(jsonBuf.Bytes(), &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v\n%s", err, jsonBuf.String())
	}
	if !env.OK {
		t.Errorf("ok = false, want true")
	}
	if env.Data != data {
		t.Errorf("json data = %+v, want the emitted value %+v", env.Data, data)
	}
	if env.Warnings == nil {
		t.Errorf("warnings must be present (non-null) on the success envelope")
	}

	var humanBuf, humanErr bytes.Buffer
	if err := output.New(&humanBuf, &humanErr, false).Emit(data); err != nil {
		t.Fatalf("human Emit: %v", err)
	}
	// The human rendering must reflect the very same values the JSON path
	// serialized.
	for _, want := range []string{env.Data.Name, fmt.Sprint(env.Data.Count)} {
		if !strings.Contains(humanBuf.String(), want) {
			t.Errorf("human output %q missing value %q from the shared data", humanBuf.String(), want)
		}
	}
}

func TestEmitWarnings(t *testing.T) {
	data := sample{Name: "x", Count: 1}

	var out, errOut bytes.Buffer
	if err := output.New(&out, &errOut, false).Emit(data, "servers.json was quarantined"); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning: servers.json was quarantined") {
		t.Errorf("human warnings must go to stderr, got stderr=%q", errOut.String())
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("human warnings must not pollute stdout, got stdout=%q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if err := output.New(&out, &errOut, true).Emit(data, "w1", "w2"); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var env struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Warnings) != 2 || env.Warnings[0] != "w1" || env.Warnings[1] != "w2" {
		t.Errorf("warnings = %v, want [w1 w2]", env.Warnings)
	}
	if errOut.Len() != 0 {
		t.Errorf("JSON mode must write everything to stdout, stderr got %q", errOut.String())
	}
}

func TestFailEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	output.New(&out, &errOut, true).Fail(output.ErrorDetail{
		Code: "E_SERVER_NOT_FOUND", Message: "no server 'gh'", Hint: "did you mean 'github'?",
	})
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Errorf("ok = true on failure envelope")
	}
	if env.Error.Code != "E_SERVER_NOT_FOUND" || env.Error.Message != "no server 'gh'" || env.Error.Hint == "" {
		t.Errorf("error object = %+v", env.Error)
	}

	out.Reset()
	errOut.Reset()
	output.New(&out, &errOut, false).Fail(output.ErrorDetail{
		Code: "E_GENERAL", Message: "boom", Hint: "try again",
	})
	if out.Len() != 0 {
		t.Errorf("human errors must go to stderr, stdout got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "agenthub: boom") || !strings.Contains(errOut.String(), "hint: try again") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// TestProgressNDJSON pins the progress contract: one compact JSON line per
// event on stdout in JSON mode (so the final envelope is simply the last
// line), and stderr-only in human mode (stdout stays reserved for results).
func TestProgressNDJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	p := output.New(&out, &errOut, true)
	p.Progress(output.ProgressEvent{
		Event:   "awaiting_browser",
		Message: "opening browser…",
		Fields:  map[string]any{"url": "https://as.example/authorize", "event": "ignored"},
	})
	if err := p.Emit(sample{Name: "x", Count: 1}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want progress line + envelope, got %d lines:\n%s", len(lines), out.String())
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("progress line is not JSON: %v", err)
	}
	if ev["event"] != "awaiting_browser" {
		t.Errorf("event = %v, want awaiting_browser (a Fields entry must never override it)", ev["event"])
	}
	if ev["url"] != "https://as.example/authorize" {
		t.Errorf("fields lost: %v", ev)
	}
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &env); err != nil || !env.OK {
		t.Errorf("last line must be the result envelope, got %q", lines[1])
	}
	if errOut.Len() != 0 {
		t.Errorf("JSON mode wrote to stderr: %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	output.New(&out, &errOut, false).Progress(output.ProgressEvent{Event: "awaiting_browser", Message: "opening browser…"})
	if out.Len() != 0 {
		t.Errorf("human progress polluted stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "opening browser") {
		t.Errorf("human progress missing from stderr: %q", errOut.String())
	}
}
