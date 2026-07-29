package registry

import (
	"encoding/json"
	"maps"
	"reflect"
	"sync"
)

// Doc is the generic persistence envelope: a typed view V plus verbatim
// passthrough of unknown JSON fields. Every structure that
// touches disk is wrapped in Doc so that fields written by a newer version
// of agenthub (or by hand) survive a load-modify-save cycle at every nesting
// level — nested structs are themselves wrapped (see ServersDoc.Servers).
//
// Invariants:
//   - Known fields (those with a json tag on T) are authoritative in V; on
//     marshal they overwrite any same-named entry captured in extra.
//   - Unknown fields round-trip byte-for-byte modulo JSON re-encoding
//     (compaction + key sorting by encoding/json).
type Doc[T any] struct {
	V     T
	extra map[string]json.RawMessage
}

// HasUnknownField reports whether the document carried a top-level field that
// T does not model. It exists for exactly one job: letting a diagnostic notice
// a RETIRED field that is still on disk.
//
// Passthrough is what makes retirement dangerous. A field the type system
// dropped keeps round-tripping verbatim, so a rule an operator wrote while it
// worked still LOOKS applied long after it stopped narrowing anything — and
// for a narrowing rule, stopping means widening. Reading the key is therefore
// deliberately all this exposes: callers may ask whether a name survived, not
// reach into the passthrough and act on it.
func (d Doc[T]) HasUnknownField(name string) bool {
	_, ok := d.extra[name]
	return ok
}

// UnmarshalJSON captures every top-level field, decodes the typed view, then
// removes the known fields so extra holds only what T does not model.
func (d *Doc[T]) UnmarshalJSON(b []byte) error {
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(b, &extra); err != nil {
		return err
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	for f := range knownJSONFields(reflect.TypeFor[T]()) {
		delete(extra, f)
	}
	d.V = v
	d.extra = extra
	return nil
}

// MarshalJSON merges unknown fields with the typed view; the typed view wins
// on key collision. Output key order is canonical (sorted) because the merge
// goes through a map.
func (d Doc[T]) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(d.extra)+8)
	maps.Copy(out, d.extra)
	kb, err := json.Marshal(d.V)
	if err != nil {
		return nil, err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(kb, &known); err != nil {
		return nil, err
	}
	maps.Copy(out, known)
	return json.Marshal(out)
}

// knownFieldsCache caches the known-JSON-field set per struct type
// (reflect.Type -> map[string]struct{}).
var knownFieldsCache sync.Map

// knownJSONFields returns the set of top-level JSON field names produced or
// consumed by t's encoding/json (mapped) representation, honoring json tags
// and promoted fields of embedded structs. Non-struct types have no known
// fields. The result is cached and must not be mutated.
func knownJSONFields(t reflect.Type) map[string]struct{} {
	if cached, ok := knownFieldsCache.Load(t); ok {
		return cached.(map[string]struct{})
	}
	fields := make(map[string]struct{})
	collectJSONFields(t, fields)
	knownFieldsCache.Store(t, fields)
	return fields
}

func collectJSONFields(t reflect.Type, out map[string]struct{}) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := cutTag(tag)
		if f.Anonymous && name == "" {
			// Embedded struct without an explicit tag name: its fields are
			// promoted to this level by encoding/json.
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectJSONFields(ft, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = struct{}{}
	}
}

// cutTag splits a json struct tag into name and options.
func cutTag(tag string) (name, opts string, hasOpts bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}
