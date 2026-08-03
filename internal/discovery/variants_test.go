package discovery

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// variantCorpus exercises every branch of the tier derivation, including the
// two that are easy to conflate: an EMPTY annotations object (the server
// described itself and said nothing about destruction → write) and a MISSING
// annotations field (the server said nothing at all → destructive).
func variantCorpus() []Tool {
	raw := []struct {
		server, tool, desc, annotations string
	}{
		{"fs", "read_file", "Read the contents of a file from disk.", `{"readOnlyHint":true}`},
		{"fs", "write_file", "Write text to a file, creating the file if needed.", `{"destructiveHint":false}`},
		{"fs", "delete_file", "Delete a file from disk permanently.", `{"destructiveHint":true}`},
		{"git", "status", "Show the working tree status of the repository.", `{}`},
		{"web", "fetch_url", "Fetch a URL over HTTP and return the response body as text.", ``},
	}
	out := make([]Tool, 0, len(raw))
	for _, r := range raw {
		exposed := r.server + "__" + r.tool
		def := mcp.ToolDef{
			Name:        exposed,
			Description: r.desc,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}
		if r.annotations != "" {
			def.Annotations = json.RawMessage(r.annotations)
		}
		out = append(out, Tool{Exposed: exposed, ServerID: r.server, RawTool: r.tool, Def: def})
	}
	return out
}

func variantSurface(t *testing.T, variants bool) *Surface {
	t.Helper()
	return New(Options{Mode: ModeLazy, Tools: variantCorpus(), IntentVariants: variants})
}

// The split surface is frozen: its six entries, their order and their
// descriptions are what a client's tool allowlist is configured against.
func TestGoldenLazyListWithVariants(t *testing.T) {
	s := variantSurface(t, true)
	assertGolden(t, "lazy_list_variants.json", marshalDefs(t, s.List()))
}

// Compatibility mode (ruling #18) must be byte-identical to the pre-variant
// surface: the switch exists so a client whose allowlist UI cannot handle
// per-tool entries keeps working.
func TestCompatModeKeepsSingleCallTool(t *testing.T) {
	names := listNames(New(Options{Mode: ModeLazy, Tools: variantCorpus()}).List())
	// Compared against the package's own base definition list, not a
	// hand-written one: the point is "the switch changes nothing", which must
	// keep holding as the meta surface grows.
	want := listNames(MetaDefs())
	if len(names) != len(want) {
		t.Fatalf("compat lazy list = %v, want the base meta-tools %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("compat lazy list = %v, want %v", names, want)
		}
	}
	for _, n := range VariantNames() {
		if slices.Contains(names, n) {
			t.Errorf("compat mode exposed the variant %q", n)
		}
	}
}

func TestVariantListReplacesCallToolExactlyOnce(t *testing.T) {
	names := listNames(variantSurface(t, true).List())
	if slices.Contains(names, MetaCallTool) {
		t.Errorf("variant mode still exposes the single %s door: %v", MetaCallTool, names)
	}
	for _, n := range VariantNames() {
		if !slices.Contains(names, n) {
			t.Errorf("variant mode does not expose %q: %v", n, names)
		}
	}
	// status and fetch_result keep their positions around the split.
	if names[0] != MetaStatus || names[1] != MetaSearchTools || names[len(names)-1] != MetaFetchResult {
		t.Errorf("variant list order drifted: %v", names)
	}
}

// Classification follows what the surface actually LISTED: a door the client
// was never shown must not be answerable, in either direction.
func TestClassifyMatchesTheListedDoors(t *testing.T) {
	split := variantSurface(t, true)
	compat := New(Options{Mode: ModeLazy, Tools: variantCorpus()})

	if got := split.Classify(MetaCallTool); got != KindUnknown {
		t.Errorf("split surface classified %s as %v, want unknown", MetaCallTool, got)
	}
	for _, n := range VariantNames() {
		if got := split.Classify(n); got != KindMeta {
			t.Errorf("split surface classified %s as %v, want meta", n, got)
		}
		if got := compat.Classify(n); got != KindUnknown {
			t.Errorf("compat surface classified %s as %v, want unknown", n, got)
		}
	}
	if got := compat.Classify(MetaCallTool); got != KindMeta {
		t.Errorf("compat surface classified %s as %v, want meta", MetaCallTool, got)
	}
}

// The variant names stay RESERVED regardless of the switch: a downstream
// tool must never be able to shadow a door a governance flip would open.
func TestVariantNamesAreAlwaysReserved(t *testing.T) {
	for _, n := range VariantNames() {
		if !IsMetaName(n) {
			t.Errorf("%q is not reserved by IsMetaName", n)
		}
	}
}

// call_with is the agent's only instruction about which door to use, so it
// must be derived from the same function the door check applies.
func TestCallWithPointsAtTheEnforcedVariant(t *testing.T) {
	s := variantSurface(t, true)
	want := map[string]string{
		"fs__read_file":   MetaCallToolRead,
		"fs__write_file":  MetaCallToolWrite,
		"fs__delete_file": MetaCallToolDestructive,
		"git__status":     MetaCallToolWrite,       // annotations object present, hints silent
		"web__fetch_url":  MetaCallToolDestructive, // no annotations at all: fail-closed
	}
	for exposed, variant := range want {
		tool, ok := s.Lookup(exposed)
		if !ok {
			t.Fatalf("%s is not visible", exposed)
		}
		if got := callWithFor(tool, true); got != variant {
			t.Errorf("call_with(%s) = %s, want %s", exposed, got, variant)
		}
		// The pointer must be accepted by the check it points at.
		if _, _, err := s.ResolveCallVariant(variant, callArgs(exposed)); err != nil {
			t.Errorf("%s rejected its own call_with %s: %v", exposed, variant, err)
		}
	}
}

// Every wrong door is rejected, and the rejection names the right one.
func TestVariantMismatchIsRejected(t *testing.T) {
	s := variantSurface(t, true)
	cases := []struct {
		exposed, used, want string
	}{
		{"fs__delete_file", MetaCallToolRead, MetaCallToolDestructive},
		{"fs__delete_file", MetaCallToolWrite, MetaCallToolDestructive},
		{"fs__read_file", MetaCallToolWrite, MetaCallToolRead},
		{"fs__read_file", MetaCallToolDestructive, MetaCallToolRead},
		{"fs__write_file", MetaCallToolRead, MetaCallToolWrite},
		{"web__fetch_url", MetaCallToolRead, MetaCallToolDestructive},
	}
	for _, tc := range cases {
		_, _, err := s.ResolveCallVariant(tc.used, callArgs(tc.exposed))
		if err == nil {
			t.Fatalf("%s through %s was accepted", tc.exposed, tc.used)
		}
		var de *Error
		if !asError(err, &de) || de.Code != CodeTierMismatch {
			t.Fatalf("%s through %s: err = %v, want code %s", tc.exposed, tc.used, err, CodeTierMismatch)
		}
		if !strings.Contains(de.Message, tc.want) {
			t.Errorf("%s through %s: message %q does not name the correct variant %s",
				tc.exposed, tc.used, de.Message, tc.want)
		}
	}
}

// The rejection wording is a contract: an agent keys its retry off it.
func TestGoldenTierMismatchWording(t *testing.T) {
	s := variantSurface(t, true)
	var b strings.Builder
	for _, tc := range []struct{ exposed, used string }{
		{"fs__delete_file", MetaCallToolRead},
		{"fs__read_file", MetaCallToolDestructive},
		{"fs__write_file", MetaCallToolRead},
		{"web__fetch_url", MetaCallToolWrite},
	} {
		_, _, err := s.ResolveCallVariant(tc.used, callArgs(tc.exposed))
		if err == nil {
			t.Fatalf("%s through %s was accepted", tc.exposed, tc.used)
		}
		b.WriteString(err.Error())
		b.WriteByte('\n')
	}
	assertGolden(t, "tier_mismatch.txt", []byte(b.String()))
}

// Compatibility mode skips the door check entirely: with one door there is
// nothing to mismatch.
func TestCompatModeSkipsTheDoorCheck(t *testing.T) {
	s := New(Options{Mode: ModeLazy, Tools: variantCorpus()})
	for _, exposed := range []string{"fs__read_file", "fs__delete_file", "web__fetch_url"} {
		if _, _, err := s.ResolveCallVariant(MetaCallTool, callArgs(exposed)); err != nil {
			t.Errorf("compat mode rejected %s: %v", exposed, err)
		}
	}
}

// An unknown tool must fail as unknown BEFORE the tier check runs: the door
// check must never become an oracle for which tools exist.
func TestUnknownToolBeatsTheDoorCheck(t *testing.T) {
	s := variantSurface(t, true)
	_, _, err := s.ResolveCallVariant(MetaCallToolRead, callArgs("fs__nope"))
	var de *Error
	if !asError(err, &de) || de.Code != CodeUnknownTool {
		t.Fatalf("err = %v, want code %s", err, CodeUnknownTool)
	}
}

// The search reply must point at the variant AND tell the agent to read
// call_with rather than to reach for a name it already knows.
func TestGoldenSearchWithVariants(t *testing.T) {
	s := variantSurface(t, true)
	res, sr := s.HandleSearch(json.RawMessage(`{"query":"delete file","limit":3}`), nil)
	if res.IsError {
		t.Fatalf("search errored: %s", res.Content)
	}
	if sr == nil || len(sr.Hits) == 0 {
		t.Fatal("search returned no hits")
	}
	assertGolden(t, "search_variants.json", indentJSON(t, res.StructuredContent))
}

func TestSearchHintDiffersByDoorShape(t *testing.T) {
	split := variantSurface(t, true).hint()
	compat := New(Options{Mode: ModeLazy, Tools: variantCorpus()}).hint()
	if split == compat {
		t.Fatal("the search hint must tell the agent which door shape it is facing")
	}
	if !strings.Contains(split, "call_with") {
		t.Errorf("variant hint %q does not mention call_with", split)
	}
	if !strings.Contains(compat, MetaCallTool+"{") {
		t.Errorf("compat hint %q does not mention %s", compat, MetaCallTool)
	}
}

// The cache key must move when the switch moves, or a governance flip would
// keep serving the old doors from a cached surface.
func TestCacheKeyTracksTheVariantSwitch(t *testing.T) {
	a := New(Options{Mode: ModeLazy, Tools: variantCorpus(), IntentVariants: true}).CacheKey()
	b := New(Options{Mode: ModeLazy, Tools: variantCorpus()}).CacheKey()
	if a == b {
		t.Fatal("cache key ignores IntentVariants; a flipped switch would serve a stale surface")
	}
}

// Grouped mode keeps the single door even with the switch on (see
// callWithFor's comment): the group listing is the contract that pins it.
func TestGroupedModeIgnoresVariants(t *testing.T) {
	s := New(Options{Mode: ModeGrouped, Tools: variantCorpus(), IntentVariants: true})
	name, ok := s.GroupName("fs")
	if !ok {
		t.Fatal("fs has no group name")
	}
	res := s.HandleGroup(name)
	if res.IsError {
		t.Fatalf("group listing errored: %s", res.Content)
	}
	var payload struct {
		Tools []struct {
			CallWith string `json:"call_with"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatal(err)
	}
	for _, e := range payload.Tools {
		if e.CallWith != MetaCallTool {
			t.Errorf("grouped call_with = %q, want %q", e.CallWith, MetaCallTool)
		}
	}
	if s.Classify(MetaCallTool) != KindMeta {
		t.Error("grouped mode must keep answering call_tool")
	}
}

// The tier ladder this package points at is internal/tier's, not a copy.
func TestToolTierDelegatesToTier(t *testing.T) {
	for _, tool := range variantCorpus() {
		if got, want := ToolTier(tool), tier.ToolTier(tool.Def.Annotations); got != want {
			t.Errorf("%s: ToolTier = %q, tier.ToolTier = %q", tool.Exposed, got, want)
		}
	}
}

func TestVariantForAndBack(t *testing.T) {
	for _, tier := range []tier.Tier{tier.Read, tier.Write, tier.Destructive} {
		name := VariantFor(tier)
		got, ok := TierOfVariant(name)
		if !ok || got != tier {
			t.Errorf("VariantFor(%q) = %q, which maps back to (%q, %v)", tier, name, got, ok)
		}
	}
	// An unrecognised tier lands on the most restricted door, never the
	// least (fail-closed).
	if got := VariantFor(tier.Tier("archive")); got != MetaCallToolDestructive {
		t.Errorf("VariantFor(unknown) = %q, want %q", got, MetaCallToolDestructive)
	}
	if _, ok := TierOfVariant(MetaCallTool); ok {
		t.Errorf("%s must not map to a tier", MetaCallTool)
	}
}

// --- helpers ---------------------------------------------------------------

func callArgs(exposed string) json.RawMessage {
	b, err := json.Marshal(CallToolArgs{Tool: exposed})
	if err != nil {
		panic(err)
	}
	return b
}

func listNames(defs []mcp.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}
