package services

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// The catalog and paste surfaces, along the same four axes as every other
// bound method (see registry_test.go): the right endpoint, the precondition
// where one exists, a forwarded refusal, and a loud failure when the daemon
// is unreachable.
func TestCatalogBoundMethods(t *testing.T) {
	runCtlCases(t, []ctlCase{
		{
			name: "SearchCatalog", method: http.MethodGet, path: "/v1/catalog",
			query: "q=github",
			data:  api.CatalogList{Query: "github", Entries: []api.CatalogEntry{{ID: "github"}}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.SearchCatalog(ctx, "github")
				return 0, err
			},
		},
		{
			// An empty query is the whole directory and must NOT send an
			// empty `q=`: "" and "no query" are the same thing here, and
			// sending the parameter anyway would make the answer's echoed
			// query differ from what the box holds.
			name: "SearchCatalog/whole directory", method: http.MethodGet, path: "/v1/catalog",
			data: api.CatalogList{Entries: []api.CatalogEntry{{ID: "github"}}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.SearchCatalog(ctx, "")
				return 0, err
			},
		},
		{
			name: "AddFromCatalog", method: http.MethodPost, path: "/v1/catalog/filesystem/add",
			guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				a, err := h.AddFromCatalog(ctx, "filesystem",
					api.CatalogAddRequest{Params: map[string]string{"directory": "/tmp"}}, readGen)
				return a.Generation, err
			},
		},
		{
			// The parser writes nothing, so it carries no precondition —
			// and a 409 from it must never be dressed up as a stale one
			// (assertConflict enforces that for every unguarded case).
			name: "ParseClientConfig", method: http.MethodPost, path: "/v1/parse/client-config",
			data: api.ParsedClientConfig{Shape: "wrapped", Servers: []api.ParsedServer{{Name: "x"}}},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.ParseClientConfig(ctx, `{"mcpServers":{}}`)
				return 0, err
			},
		},
	})
}

// The add request must reach the daemon with the parameters the user typed.
// A dropped parameter would store an entry with an unresolved {{placeholder}}
// in its command line, which fails much later with a path nobody typed.
func TestCatalogAddCarriesItsParameters(t *testing.T) {
	rec := &ctlRecorder{respond: okMode, data: writeAnswer()}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	if _, err := h.AddFromCatalog(t.Context(), "filesystem", api.CatalogAddRequest{
		Name:   "files",
		Params: map[string]string{"directory": "/srv/data"},
	}, readGen); err != nil {
		t.Fatalf("AddFromCatalog: %v", err)
	}

	var body struct {
		Name   string            `json:"name"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(rec.last(t).body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body.Name != "files" {
		t.Errorf("name = %q, want %q", body.Name, "files")
	}
	if body.Params["directory"] != "/srv/data" {
		t.Errorf("params = %v, want directory=/srv/data", body.Params)
	}
}

// The pasted text travels verbatim. A parser that received a trimmed or
// re-encoded document would report findings about a document the user never
// had, and the preview is the only thing standing between a clipboard and
// the registry.
func TestParseClientConfigSendsTheTextVerbatim(t *testing.T) {
	rec := &ctlRecorder{respond: okMode, data: api.ParsedClientConfig{Shape: "single-entry"}}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	const pasted = "{\n  \"command\": \"npx\",\n  \"args\": [\"-y\", \"pkg\"]\n}\n"
	if _, err := h.ParseClientConfig(t.Context(), pasted); err != nil {
		t.Fatalf("ParseClientConfig: %v", err)
	}
	got := rec.last(t)
	if got.query != "" {
		t.Errorf("query = %q: a parse writes nothing and carries no precondition", got.query)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body.Text != pasted {
		t.Errorf("text = %q, want %q", body.Text, pasted)
	}
}

// codeUnsupportedFormat mirrors internal/ctlapi.CodeUnsupportedFormat. It is
// spelled out rather than imported because this package may import only the
// public api package (canonical.md §2 rule 1), and api has no constant for
// it — the wire value is the contract either way.
const codeUnsupportedFormat = "E_UNSUPPORTED_FORMAT"

// A format agenthub recognizes but does not parse answers E_UNSUPPORTED_FORMAT
// with the manual route in its hint. Both have to survive the binding: the
// code is what tells the dialog this is not "your paste is broken", and the
// hint is the only thing that tells the user what to do instead.
func TestUnsupportedFormatKeepsItsHint(t *testing.T) {
	const hint = "add it by hand: agenthub server add <id> --cmd …"
	rec := &ctlRecorder{data: nil}
	rec.respond = func(t *testing.T, w http.ResponseWriter, _ any) {
		t.Helper()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"ok": false,
			"error": map[string]any{
				"code":    codeUnsupportedFormat,
				"message": "TOML configuration is not parsed by agenthub",
				"hint":    hint,
			},
		}); err != nil {
			t.Errorf("encoding refusal: %v", err)
		}
	}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	_, err := h.ParseClientConfig(t.Context(), "[mcp_servers.x]\ncommand = \"npx\"\n")
	if !api.IsCode(err, codeUnsupportedFormat) {
		t.Fatalf("want %s, got %v", codeUnsupportedFormat, err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(MarshalError(err), &m); uerr != nil {
		t.Fatalf("MarshalError produced invalid JSON: %v", uerr)
	}
	if m["code"] != codeUnsupportedFormat {
		t.Errorf("code = %v, want %s", m["code"], codeUnsupportedFormat)
	}
	if h, _ := m["hint"].(string); !strings.Contains(h, "server add") {
		t.Errorf("hint = %v: the manual route must survive to the dialog", m["hint"])
	}
}
