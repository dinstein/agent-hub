package gateway

import (
	"reflect"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// specEqual and dockerEqual decide whether a reload may KEEP an existing
// downstream connection. Their doc comments demand that every
// connection-relevant field appear in them, and nothing checked it: a field
// added to downstream.Spec or transport.DockerConfig and then populated by
// SpecFromEntry, but never compared, leaves the old connection running under
// the old definition. For the container block that is the failure AGENTS.md
// names outright — an operator changes the image, the mounts or the network
// and keeps the previous isolation until something restarts the gateway.
//
// The rule was review-only, and the review it invites is expensive: proving
// the two functions complete means tracing which fields SpecFromEntry
// actually fills, through downstream/entry.go, downstream/spec.go and
// downstream/derive.go. That trace is what these tables record.
//
// The tests below are behavioural, not a list compared against a list: for
// each field they build two values differing in that field ALONE and assert
// what the comparison does with it. A field named in the exempt table is
// asserted to be ignored, so the table cannot rot in either direction — an
// exemption that starts being compared fails just as loudly as a compared
// field that stops being.

// specExempt are the downstream.Spec fields specEqual deliberately ignores,
// each with the reason it cannot reach this comparison.
//
// Both are written by the DERIVATION layer (downstream/derive.go), never by
// SpecFromEntry, so on the two specs this comparison actually receives — the
// applied set and the one just read from the registry — they are equal and
// empty on both sides. Comparing them would be dead weight, and leaving them
// out silently is what makes the next reader repeat the trace.
var specExempt = map[string]string{
	"DeriveKey": "set only by derive.go; a registry-derived spec is always the base instance",
	"ScopeName": "set only by derive.go, to the derive key; empty on every registry-derived spec",
}

// dockerExempt are the transport.DockerConfig fields dockerEqual ignores.
// SpecFromEntry fills exactly the eight fields dockerEqual compares (see
// downstream/entry.go); these five arrive later or not at all, so a registry
// edit can never move them.
var dockerExempt = map[string]string{
	"Env":           "filled at connect time from resolved secrets (downstream/spec.go), not from the entry",
	"ServerID":      "stamped at connect time from Spec.ID, which specEqual already compares",
	"ContainerName": "generated unique per spawn on purpose; never read from the registry",
	"CIDFile":       "generated under TempDir when empty; never read from the registry",
	"Binary":        "docker CLI discovery override, not a per-server registry field",
}

func TestSpecEqualComparesEveryConnectionRelevantField(t *testing.T) {
	assertFieldCoverage(t, reflect.TypeOf(downstream.Spec{}), specExempt,
		func(a, b reflect.Value) bool {
			return specEqual(a.Interface().(downstream.Spec), b.Interface().(downstream.Spec))
		})
}

func TestDockerEqualComparesEveryConnectionRelevantField(t *testing.T) {
	assertFieldCoverage(t, reflect.TypeOf(transport.DockerConfig{}), dockerExempt,
		func(a, b reflect.Value) bool {
			return dockerEqual(a.Addr().Interface().(*transport.DockerConfig),
				b.Addr().Interface().(*transport.DockerConfig))
		})
}

// assertFieldCoverage walks every exported field of typ, builds two values
// differing in that field alone, and checks equal() against the exempt table.
func assertFieldCoverage(t *testing.T, typ reflect.Type, exempt map[string]string,
	equal func(a, b reflect.Value) bool,
) {
	t.Helper()
	seen := map[string]bool{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		seen[f.Name] = true

		a := reflect.New(typ).Elem()
		b := reflect.New(typ).Elem()
		setNonZero(t, b.Field(i), f.Name)

		reason, isExempt := exempt[f.Name]
		switch got := equal(a, b); {
		case isExempt && !got:
			t.Errorf("%s.%s is listed as exempt (%s), but the comparison DOES read it.\n"+
				"Drop it from the exempt table — an exemption that no longer holds hides "+
				"the field from the next reader for no reason.", typ.Name(), f.Name, reason)
		case !isExempt && got:
			t.Errorf("%s.%s differs between the two values and the comparison still reports them equal.\n"+
				"A field compared nowhere is a field whose edit leaves the old connection running "+
				"under the old definition. Compare it, or add it to the exempt table in this file "+
				"with the reason a registry edit can never move it.", typ.Name(), f.Name)
		}
	}
	// An exemption for a field that no longer exists outlives the reason it
	// was written for, and reads as coverage of something.
	for name := range exempt {
		if !seen[name] {
			t.Errorf("%s has no field %q, but the exempt table still names it", typ.Name(), name)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("%s exposed no exported fields; the walk is wrong", typ.Name())
	}
}

// setNonZero gives f a value distinguishable from its zero, whatever its
// shape. A field this cannot fill is a field the coverage check would skip
// silently, so it fails hard instead.
func setNonZero(t *testing.T, f reflect.Value, name string) {
	t.Helper()
	switch f.Kind() {
	case reflect.String:
		f.SetString("x")
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f.SetUint(1)
	case reflect.Map:
		m := reflect.MakeMap(f.Type())
		m.SetMapIndex(nonZeroOf(t, f.Type().Key(), name), nonZeroOf(t, f.Type().Elem(), name))
		f.Set(m)
	case reflect.Slice:
		s := reflect.MakeSlice(f.Type(), 1, 1)
		setNonZero(t, s.Index(0), name)
		f.Set(s)
	case reflect.Pointer:
		f.Set(reflect.New(f.Type().Elem()))
	case reflect.Struct:
		// One differing member is enough to make the struct differ; the
		// member's own coverage is that type's business.
		for i := range f.NumField() {
			if f.Field(i).CanSet() {
				setNonZero(t, f.Field(i), name)
				return
			}
		}
		t.Fatalf("field %s: struct %s has no settable member", name, f.Type())
	default:
		t.Fatalf("field %s: cannot build a non-zero %s; teach setNonZero about it "+
			"rather than letting the field go unchecked", name, f.Kind())
	}
}

func nonZeroOf(t *testing.T, typ reflect.Type, name string) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	setNonZero(t, v, name)
	return v
}
