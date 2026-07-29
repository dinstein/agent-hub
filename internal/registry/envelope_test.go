package registry

import (
	"encoding/json"
	"reflect"
	"testing"
)

type embedded struct {
	Inner string `json:"inner"`
}

type tagged struct {
	embedded
	Plain   string `json:"plain"`
	Renamed string `json:"other_name,omitempty"`
	Ignored string `json:"-"`
	NoTag   string
	hidden  string //nolint:unused // present to prove unexported fields are skipped
}

func TestKnownJSONFields(t *testing.T) {
	got := knownJSONFields(reflect.TypeFor[tagged]())
	want := []string{"inner", "plain", "other_name", "NoTag"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing known field %q in %v", name, got)
		}
	}
	if _, ok := got["Ignored"]; ok {
		t.Error("json:\"-\" field must not be known")
	}
	// Cache must return the identical set on second call.
	again := knownJSONFields(reflect.TypeFor[tagged]())
	if !reflect.DeepEqual(got, again) {
		t.Error("cache returned a different set")
	}
}

func TestDocRoundTripPreservesUnknownFieldsAtAllLevels(t *testing.T) {
	in := []byte(`{
		"servers": {
			"alpha": {
				"transport": "stdio",
				"command": "npx",
				"enabled": true,
				"entry_unknown": {"deep": [1, 2]}
			}
		},
		"top_unknown": "keep-me"
	}`)

	var doc Doc[ServersDoc]
	if err := json.Unmarshal(in, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.V.Servers["alpha"].V.Command != "npx" {
		t.Fatalf("typed view lost data: %+v", doc.V)
	}

	// Modify a known field; unknown fields must survive.
	e := doc.V.Servers["alpha"]
	e.V.Enabled = false
	doc.V.Servers["alpha"] = e

	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["top_unknown"] != "keep-me" {
		t.Errorf("top-level unknown field lost: %s", out)
	}
	alpha := m["servers"].(map[string]any)["alpha"].(map[string]any)
	if _, ok := alpha["entry_unknown"]; !ok {
		t.Errorf("nested unknown field lost: %s", out)
	}
	if alpha["enabled"] != false {
		t.Errorf("known-field edit lost: %s", out)
	}
}

func TestDocKnownFieldWinsOverStaleExtra(t *testing.T) {
	// A field that is known to T must never be resurrected from extra.
	var doc Doc[MetaDoc]
	if err := json.Unmarshal([]byte(`{"generation": 7, "note": "x"}`), &doc); err != nil {
		t.Fatal(err)
	}
	doc.V.Generation = 8
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Generation uint64 `json:"generation"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.Generation != 8 || m.Note != "x" {
		t.Fatalf("got %+v from %s", m, out)
	}
}
