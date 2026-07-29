package ctlapi

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/catalog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// catalogListBody mirrors catalogListWire for decoding.
type catalogListBody struct {
	Query   string `json:"query"`
	Entries []struct {
		catalog.Entry
		NeedsConfig  bool     `json:"needs_config"`
		RequiredKeys []string `json:"required_keys"`
	} `json:"entries"`
}

func TestCatalogListAndSearch(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	all := doAdmin(t, env.sock, http.MethodGet, "/v1/catalog", nil)
	if all.status != http.StatusOK {
		t.Fatalf("list: %d %s", all.status, all.raw)
	}
	var full catalogListBody
	all.decode(t, &full)
	if len(full.Entries) != len(catalog.List()) {
		t.Fatalf("list returned %d entries, want %d", len(full.Entries), len(catalog.List()))
	}
	if full.Query != "" {
		t.Errorf("query = %q, want empty", full.Query)
	}
	// needs_config is computed server-side: a frontend must never have to
	// re-derive the one-click split.
	var sawOneClick, sawNeedsConfig bool
	for _, e := range full.Entries {
		if e.NeedsConfig {
			sawNeedsConfig = true
		} else {
			sawOneClick = true
		}
	}
	if !sawOneClick || !sawNeedsConfig {
		t.Error("the seed must contain both one-click and configuring entries")
	}

	hit := doAdmin(t, env.sock, http.MethodGet, "/v1/catalog?q=github", nil)
	var found catalogListBody
	hit.decode(t, &found)
	if found.Query != "github" {
		t.Errorf("query = %q, want it echoed", found.Query)
	}
	if len(found.Entries) == 0 || found.Entries[0].ID != "github" {
		t.Fatalf("search github = %+v", found.Entries)
	}

	miss := doAdmin(t, env.sock, http.MethodGet, "/v1/catalog?q=zzzz-no-such-server", nil)
	var none catalogListBody
	miss.decode(t, &none)
	if len(none.Entries) != 0 {
		t.Errorf("entries = %+v, want none", none.Entries)
	}
}

func TestCatalogGet(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	got := doAdmin(t, env.sock, http.MethodGet, "/v1/catalog/filesystem", nil)
	var entry struct {
		catalog.Entry
		NeedsConfig bool `json:"needs_config"`
	}
	got.decode(t, &entry)
	if entry.ID != "filesystem" || !entry.NeedsConfig || len(entry.Params) == 0 {
		t.Fatalf("entry = %+v", entry)
	}

	// An unknown id reads exactly like an unknown route (anti-probing).
	doAdmin(t, env.sock, http.MethodGet, "/v1/catalog/nope", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
	// Wrong method, same answer.
	doAdmin(t, env.sock, http.MethodDelete, "/v1/catalog/filesystem", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

// catalogAddBody mirrors catalogAddResult for decoding.
type catalogAddBody struct {
	Generation uint64                `json:"generation"`
	Changed    bool                  `json:"changed"`
	ID         string                `json:"id"`
	CatalogID  string                `json:"catalog_id"`
	Entry      *registry.ServerEntry `json:"entry"`
	NextSteps  []string              `json:"next_steps"`
}

// The one-click path: no body at all, and the server lands in the registry
// under its catalog id.
func TestCatalogAddOneClick(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	res := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/fetch/add", nil)
	if res.status != http.StatusOK {
		t.Fatalf("add: %d %s", res.status, res.raw)
	}
	var added catalogAddBody
	res.decode(t, &added)
	if added.ID != "fetch" || added.CatalogID != "fetch" || !added.Changed || added.Generation == 0 {
		t.Fatalf("add result = %+v", added)
	}
	stored, ok := env.reg.Snapshot().Servers.V.Servers["fetch"]
	if !ok {
		t.Fatal("server not in registry")
	}
	if stored.V.Source != "catalog:fetch" || !stored.V.Enabled {
		t.Errorf("stored entry = %+v", stored.V)
	}
	if len(added.NextSteps) != 0 {
		t.Errorf("next steps = %v, want none for a credential-free entry", added.NextSteps)
	}

	// Audited under the catalog verb, with the server it created.
	recs := findAudit(env.aud.records(), "catalog/add:fetch")
	if len(recs) != 1 {
		t.Fatalf("want one audit record, got %d", len(recs))
	}
	if recs[0].Server != "fetch" || recs[0].Actor != "gui" || recs[0].RequestID == "" {
		t.Errorf("audit record = %+v", recs[0])
	}
	if recs[0].Decision != audit.DecisionAllowed {
		t.Errorf("decision = %q", recs[0].Decision)
	}
}

// The configuring path: parameters are substituted, the name may be
// overridden, and the credential still to be stored comes back as a command.
func TestCatalogAddWithParamsAndRename(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	res := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/filesystem/add", map[string]any{
		"name":   "work-files",
		"params": map[string]string{"directory": "/tmp/work"},
	})
	if res.status != http.StatusOK {
		t.Fatalf("add: %d %s", res.status, res.raw)
	}
	var added catalogAddBody
	res.decode(t, &added)
	if added.ID != "work-files" || added.CatalogID != "filesystem" {
		t.Fatalf("add result = %+v", added)
	}
	stored := env.reg.Snapshot().Servers.V.Servers["work-files"].V
	if !slices.Contains(stored.Args, "/tmp/work") {
		t.Errorf("parameter not substituted: %+v", stored.Args)
	}

	// The credential half, on a different entry: no curated entry declares a
	// parameter and a credential together any more, so the secret reference
	// and the next step it produces are checked where one exists. That a
	// substitution and a secret reference coexist safely in one definition is
	// catalog.TestRenderSubstitutesAndRefuses.
	res = doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/brave-search/add", map[string]any{
		"name": "web-search",
	})
	if res.status != http.StatusOK {
		t.Fatalf("add: %d %s", res.status, res.raw)
	}
	var keyed catalogAddBody
	res.decode(t, &keyed)
	keptSecret := env.reg.Snapshot().Servers.V.Servers["web-search"].V
	// A secret reference must reach the registry VERBATIM: resolving it
	// here would put a credential into a registry document.
	if keptSecret.Env["BRAVE_API_KEY"] != "${SECRET_BRAVE_API_KEY}" {
		t.Errorf("secret placeholder mangled: %q", keptSecret.Env["BRAVE_API_KEY"])
	}
	want := "agenthub secret set web-search BRAVE_API_KEY"
	if !slices.Contains(keyed.NextSteps, want) {
		t.Errorf("next steps = %v, want %q", keyed.NextSteps, want)
	}
}

// An OAuth entry is still one-click; the login is the next step, named.
func TestCatalogAddOAuthEntryReportsTheLogin(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	res := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/sentry/add", nil)
	var added catalogAddBody
	res.decode(t, &added)
	if !slices.Contains(added.NextSteps, "agenthub auth login sentry") {
		t.Errorf("next steps = %v", added.NextSteps)
	}
	if added.Entry == nil || added.Entry.Provenance != registry.ProvenanceRemote {
		t.Errorf("a curated endpoint must stay screened: %+v", added.Entry)
	}
}

// Incomplete parameters are refused BEFORE the registry is opened, with the
// missing names and the declared ones both in the answer.
func TestCatalogAddRefusesIncompleteParameters(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	res := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/filesystem/add", nil)
	res.wantErr(t, http.StatusBadRequest, CodeBadRequest)
	if !strings.Contains(res.Error.Message, "directory") {
		t.Errorf("message = %q, want the missing parameter named", res.Error.Message)
	}
	if !strings.Contains(res.Error.Hint, "directory") {
		t.Errorf("hint = %q, want the declared parameters listed", res.Error.Hint)
	}
	if _, ok := env.reg.Snapshot().Servers.V.Servers["filesystem"]; ok {
		t.Error("a refused add left an entry behind")
	}

	unknown := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/filesystem/add", map[string]any{
		"params": map[string]string{"directory": "/tmp", "nope": "1"},
	})
	unknown.wantErr(t, http.StatusBadRequest, CodeBadRequest)
	if !strings.Contains(unknown.Error.Message, "nope") {
		t.Errorf("message = %q, want the unknown parameter named", unknown.Error.Message)
	}
}

func TestCatalogAddFailureModes(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	// Unknown catalog id: the uniform 404.
	doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/nope/add", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)

	// A name already taken is a conflict, not a silent replacement.
	doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/fetch/add", nil)
	dup := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/fetch/add", nil)
	dup.wantErr(t, http.StatusConflict, confops.CodeServerExists)
	// A refused write is audited too.
	var denied bool
	for _, r := range findAudit(env.aud.records(), "catalog/add:fetch") {
		if r.Decision == audit.DecisionDenied {
			denied = true
		}
	}
	if !denied {
		t.Error("the refused add was not audited")
	}

	// A stale precondition is the ordinary 409 with the current generation.
	stale := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/memory/add", map[string]any{
		"expected_generation": 99,
	})
	stale.wantErr(t, http.StatusConflict, CodeStalePrecondition)
	if stale.Error.Generation == 0 {
		t.Error("the stale answer must carry the current generation")
	}

	// A correct precondition goes through.
	gen := env.reg.Snapshot().Generation
	ok := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/memory/add", map[string]any{
		"expected_generation": gen,
	})
	if ok.status != http.StatusOK {
		t.Fatalf("guarded add: %d %s", ok.status, ok.raw)
	}

	// Malformed body.
	doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/time/add", "{not json").
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
}

// Every curated entry must survive the SAME write path a hand-typed one
// takes — no shortcut through confops validation.
func TestEveryCuratedEntryCanBeAdded(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	for _, e := range catalog.List() {
		params := map[string]string{}
		for _, p := range e.Params {
			v := p.Example
			if v == "" {
				v = "value"
			}
			params[p.Name] = v
		}
		res := doAdmin(t, env.sock, http.MethodPost, "/v1/catalog/"+e.ID+"/add",
			map[string]any{"params": params})
		if res.status != http.StatusOK {
			t.Errorf("%s: add: %d %s", e.ID, res.status, res.raw)
		}
	}
}
