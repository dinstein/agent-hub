package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/registry"
)

// TestPasteRoutesAgreeOnWholeSnippets drives complete client-configuration
// documents through BOTH paste routes and compares the entries they produce.
//
// The spelling table has its own parity test (stdintransport_test.go), written
// after the two routes disagreed about how a transport is written. Sharing one
// table fixed that question and no other: the routes then drifted on the rest
// of the mapping, in every direction a shared constant does not police —
// --stdin modelled no "disabled" / "enabled" key and rejected any snippet
// carrying one, knew a single wrapper key so a VS Code {"mcp":{"servers":…}}
// fragment failed as a server named "mcp", and read only the "url" spelling.
// A test over markers cannot see any of that. This one takes whole snippets,
// which is what a user actually pastes.
//
// EVERYTHING about the resulting entry is compared except three fields, each
// exempt for a stated reason and each still checked, separately, on its own
// terms below:
//
//   - Source: "pasted" vs "manual" records who is adding the server, which the
//     snippet says nothing about.
//   - Enabled: `server add` lands every server switched off whatever the
//     configuration claims (`server enable` is the separate step that probes),
//     while the preview shows what the snippet says. What the snippet says is
//     asserted on the preview side, per case — that reading is the gap this
//     branch closed, so it is pinned rather than waved through.
//   - Provenance: empty and ProvenanceRemote mean the same screening
//     (registry/types.go). They are folded together, not ignored: a "local"
//     provenance appearing on one side alone still fails here.
//
// Adding a fourth exemption means editing this comment, which is the point.
func TestPasteRoutesAgreeOnWholeSnippets(t *testing.T) {
	cases := []struct {
		name string
		// nameArg is the name `server add --stdin` is invoked with, needed
		// for the single-entry shape (which names nothing) and left empty
		// otherwise. The preview never takes one, so entries are compared by
		// position when it is set.
		nameArg string
		snippet string
		// wantEnabled is what the SNIPPET's switch says, as the preview
		// reports it. The CLI's own answer is always false.
		wantEnabled bool
	}{{
		name:        "claude wrapper, stdio with args env cwd",
		snippet:     `{"mcpServers":{"linear":{"command":"npx","args":["-y","linear-mcp"],"env":{"API_KEY":"${SECRET_LINEAR}"},"cwd":"/srv"}}}`,
		wantEnabled: true,
	}, {
		name:        "vs code nested mcp.servers section",
		snippet:     `{"mcp":{"servers":{"gh":{"type":"http","url":"https://api.example.com/mcp","headers":{"X-Api":"${SECRET_GH}"}}}}}`,
		wantEnabled: true,
	}, {
		name:        "zed context_servers, positive switch off",
		snippet:     `{"context_servers":{"z":{"command":"z-mcp","enabled":false}}}`,
		wantEnabled: false,
	}, {
		name:        "zed context_servers, positive switch on",
		snippet:     `{"context_servers":{"z":{"command":"z-mcp","enabled":true}}}`,
		wantEnabled: true,
	}, {
		name:        "cline negative switch off",
		snippet:     `{"mcpServers":{"c":{"command":"c-bin","disabled":true}}}`,
		wantEnabled: false,
	}, {
		name:        "cline negative switch on",
		snippet:     `{"mcpServers":{"c":{"command":"c-bin","disabled":false}}}`,
		wantEnabled: true,
	}, {
		name:        "both switches, disagreeing, lands off",
		snippet:     `{"mcpServers":{"c":{"command":"c-bin","disabled":false,"enabled":false}}}`,
		wantEnabled: false,
	}, {
		name:        "serverUrl spelling",
		snippet:     `{"mcpServers":{"s":{"type":"sse","serverUrl":"https://s.example.com/sse"}}}`,
		wantEnabled: true,
	}, {
		name:        "httpUrl spelling",
		snippet:     `{"mcpServers":{"s":{"httpUrl":"https://s.example.com/mcp"}}}`,
		wantEnabled: true,
	}, {
		name:        "transport marker folding",
		snippet:     `{"mcpServers":{"a":{"transport":"StreamableHTTP","url":"https://x.example.com/mcp"}}}`,
		wantEnabled: true,
	}, {
		name:        "oauth login hints",
		snippet:     `{"mcpServers":{"elk":{"transport":"sse","url":"https://elk.example.com/sse","oauth":{"issuer":"https://as.example.com","scopes":["openid","read"]}}}}`,
		wantEnabled: true,
	}, {
		name:        "bare name -> entry map",
		snippet:     `{"foo":{"command":"foo-bin"},"bar":{"command":"bar-bin"}}`,
		wantEnabled: true,
	}, {
		name:        "single unnamed entry",
		nameArg:     "xsrv",
		snippet:     `{"command":"npx","args":["-y","x-mcp"]}`,
		wantEnabled: true,
	}, {
		name:        "several servers in one wrapper",
		snippet:     `{"mcpServers":{"b":{"command":"b-bin"},"a":{"url":"https://a.example.com/mcp"}}}`,
		wantEnabled: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := catalog.ParseClientConfig(tc.snippet)
			if err != nil {
				t.Fatalf("the preview route refused a snippet --stdin is expected to take: %v", err)
			}
			if len(preview.Skipped) != 0 {
				t.Fatalf("preview skipped %+v", preview.Skipped)
			}
			written, err := normalizeStdin([]byte(tc.snippet), tc.nameArg)
			if err != nil {
				t.Fatalf("--stdin refused a snippet the preview accepts: %v", err)
			}
			if len(written) != len(preview.Servers) {
				t.Fatalf("--stdin produced %d entries, the preview %d", len(written), len(preview.Servers))
			}
			for i, want := range preview.Servers {
				got := written[i]
				if tc.nameArg == "" && got.Name != want.Name {
					t.Errorf("entry %d: --stdin named it %q, the preview %q", i, got.Name, want.Name)
				}
				if want.Entry.Enabled != tc.wantEnabled {
					t.Errorf("entry %d: the preview read the snippet's switch as enabled=%v, want %v",
						i, want.Entry.Enabled, tc.wantEnabled)
				}
				if got.Entry.Enabled {
					t.Errorf("entry %d: --stdin added an ENABLED server; `add` writes configuration "+
						"only and `server enable` is the step that probes", i)
				}
				if a, b := canonicalForParity(got.Entry), canonicalForParity(want.Entry); !reflect.DeepEqual(a, b) {
					t.Errorf("entry %d: the two paste routes disagree\n--stdin : %+v\npreview : %+v", i, a, b)
				}
			}
		})
	}
}

// canonicalForParity zeroes the three fields the two routes are allowed to
// differ on, and only those. See the comment on the test above for why each
// one is exempt.
func canonicalForParity(e registry.ServerEntry) registry.ServerEntry {
	e.Source = ""
	e.Enabled = false
	if e.Provenance == registry.ProvenanceRemote {
		e.Provenance = ""
	}
	return e
}

// TestPasteRoutesAgreeOnWhatTheyRefuse is the other half: a document neither
// route can use must fail on both, or the same paste succeeds in the GUI and
// errors in the terminal (or, worse, the reverse).
//
// The wording is deliberately not compared — the CLI wraps a refusal in its
// own error codes, and pinning message text here would freeze two files
// together for no gain.
func TestPasteRoutesAgreeOnWhatTheyRefuse(t *testing.T) {
	cases := []struct{ name, nameArg, snippet string }{
		{"unknown transport marker", "", `{"mcpServers":{"r":{"type":"grpc","url":"https://x.example.com/mcp"}}}`},
		{"typo'd transport marker", "", `{"mcpServers":{"r":{"type":"htpp","command":"npx","url":"https://x.example.com/mcp"}}}`},
		{"remote entry with no url", "", `{"mcpServers":{"r":{"type":"http","command":"c"}}}`},
		{"entry naming neither command nor url", "", `{"mcpServers":{"r":{"args":["a"]}}}`},
		{"zed's own command-object shape", "", `{"context_servers":{"z":{"source":"custom","command":{"path":"z-mcp"}}}}`},
		{"not a server configuration at all", "", `{"editor":{"fontSize":12}}`},
		{"empty wrapper", "", `{"mcpServers":{}}`},
		{"agenthub's own gateway entry", "", `{"mcpServers":{"agenthub":{"command":"/usr/local/bin/agenthub","args":["connect","--client","cursor"]}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, previewErr := catalog.ParseClientConfig(tc.snippet)
			// The preview reports an unusable entry as a skip rather than as
			// an error when the rest of the paste survives; with one entry in
			// the document, "nothing usable" is the same refusal.
			previewRefused := previewErr != nil || len(res.Servers) == 0
			_, writeErr := normalizeStdin([]byte(tc.snippet), tc.nameArg)
			if previewRefused != (writeErr != nil) {
				t.Fatalf("the routes disagree: preview refused = %v (%v), --stdin refused = %v (%v)",
					previewRefused, previewErr, writeErr != nil, writeErr)
			}
		})
	}
}

// TestUnknownFieldDirectionIsTheOnlyDeliberateDifference pins the exemption
// itself, so "the routes agree" above cannot be read as "the routes are the
// same function".
//
// A key agenthub does not model is a WARNING on the preview and an ERROR on
// --stdin (docs/modules/controlplane.md). The preview shows the user exactly
// what would be stored, so "these keys were ignored" is actionable; a write
// with no preview can only refuse, or the user never learns that the block
// they pasted vanished. Both directions are asserted here: a test that only
// checked the error would pass if the preview started refusing too, and the
// preview refusing a README fragment over a "timeout" key is the failure this
// whole route exists to avoid.
func TestUnknownFieldDirectionIsTheOnlyDeliberateDifference(t *testing.T) {
	const snippet = `{"mcpServers":{"s":{"command":"npx","alwaysAllow":["x"],"timeout":60}}}`

	res, err := catalog.ParseClientConfig(snippet)
	if err != nil {
		t.Fatalf("the preview refused an unmodeled key; it must warn: %v", err)
	}
	warn := strings.Join(res.Servers[0].Warnings, " | ")
	if !strings.Contains(warn, "alwaysAllow") || !strings.Contains(warn, "timeout") {
		t.Errorf("warnings = %q, want both ignored keys named", warn)
	}
	if _, err := normalizeStdin([]byte(snippet), ""); err == nil {
		t.Fatal("--stdin accepted an unmodeled key; it would have been dropped in silence")
	}
}
