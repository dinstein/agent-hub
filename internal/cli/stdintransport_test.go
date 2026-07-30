package cli

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/registry"
)

// TestNormalizeStdinAgreesWithTheCatalogOnSpellings pins that the two paste
// paths resolve a transport marker identically.
//
// `agenthub catalog` and `server add --stdin` read the same kind of pasted
// snippet, and they used to disagree. The catalog folded case and accepted
// "local", "command", "streamable_http" and "remote"; the CLI carried a
// narrower, case-SENSITIVE copy that knew only streamable-http, streamableHttp
// and http-stream. So a config one command accepted, the other refused, for no
// reason a user could infer from either.
//
// Both now call catalog.TransportFromSpelling. This test is what keeps them
// from drifting apart again: it drives the shared table's whole input set
// through normalizeStdin, so re-inlining a private copy in the CLI fails here
// rather than in somebody's paste.
//
// The doc comment on normalizeStdin also used to claim "Only stdio entries are
// accepted in M0", which was false in the other direction — sse and http
// worked, and the OAuth-hints test above pastes an sse entry and expects
// success.
func TestNormalizeStdinAgreesWithTheCatalogOnSpellings(t *testing.T) {
	// Every spelling the shared table folds, with the transport it means.
	spellings := map[string]string{
		"stdio": registry.TransportStdio, "local": registry.TransportStdio,
		"command": registry.TransportStdio,
		"sse":     registry.TransportSSE,
		"http":    registry.TransportHTTP, "streamable-http": registry.TransportHTTP,
		"streamablehttp": registry.TransportHTTP, "streamable_http": registry.TransportHTTP,
		"http-stream": registry.TransportHTTP, "remote": registry.TransportHTTP,
		// Case folding is part of the contract, not an accident.
		"STDIO": registry.TransportStdio, "Http": registry.TransportHTTP,
		"StreamableHTTP": registry.TransportHTTP, " sse ": registry.TransportSSE,
	}

	for marker, want := range spellings {
		t.Run("marker="+marker, func(t *testing.T) {
			// The shared table is the reference; assert it first so a failure
			// says which side moved.
			if got := catalog.TransportFromSpelling(marker); got != want {
				t.Fatalf("catalog.TransportFromSpelling(%q) = %q, want %q", marker, got, want)
			}
			// A url is supplied for every case: it is ignored for stdio (which
			// takes the command) and required by the remote transports.
			in := []byte(`{"mcpServers":{"a":{"transport":"` + marker + `",` +
				`"command":"npx","url":"https://x.example.com/mcp"}}}`)
			got, err := normalizeStdin(in, "")
			if err != nil {
				t.Fatalf("normalizeStdin with transport %q = %v, want it accepted "+
					"(the catalog accepts this spelling)", marker, err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if got[0].Entry.Transport != want {
				t.Errorf("transport = %q, want %q — the two paste paths disagree again",
					got[0].Entry.Transport, want)
			}
		})
	}
}

// TestNormalizeStdinRefusesAnUnknownMarkerRatherThanGuessing keeps the failure
// direction of the shared table.
//
// An unrecognized marker must NOT fall back to inference. "htpp" is a typo for
// http, and a fallback would resolve it to stdio because a command is present —
// turning a remote server into a local process launch, which is the one wrong
// answer that executes something.
func TestNormalizeStdinRefusesAnUnknownMarkerRatherThanGuessing(t *testing.T) {
	if got := catalog.TransportFromSpelling("htpp"); got != "" {
		t.Fatalf("TransportFromSpelling(\"htpp\") = %q, want \"\"", got)
	}
	in := []byte(`{"mcpServers":{"a":{"transport":"htpp","command":"npx",` +
		`"url":"https://x.example.com/mcp"}}}`)
	if _, err := normalizeStdin(in, ""); err == nil {
		t.Fatal("a typo'd transport was accepted; with a command present it would " +
			"have become a local process launch")
	}
}
