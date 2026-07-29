package skills

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// decodeText extracts the single text item of a provider CallResult.
func decodeText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		t.Fatalf("unexpected content %s (err %v)", raw, err)
	}
	return items[0].Text
}

// TestProviderExposesEnabledSkillsOnly is the visibility rule of the MCP
// supply face: enabled skills are tools, disabled ones are INVISIBLE (not
// listed-and-refused — same anti-probing direction as scope narrowing).
func TestProviderExposesEnabledSkillsOnly(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	sk, _ := addSample(t, m)

	p := NewProvider(m)
	if len(p.Tools()) != 0 {
		t.Fatal("a provider must expose nothing before its first Refresh")
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	raw := RawToolName(sk.ID)
	if got := p.ToolNames(); !slices.Equal(got, []string{raw}) {
		t.Fatalf("tools = %v, want [%s]", got, raw)
	}
	def := p.Tools()[0]
	if def.Name != raw || def.Title != sk.Name {
		t.Fatalf("tool def = %+v", def)
	}
	if !strings.Contains(def.Description, sk.ID) || !strings.Contains(def.Description, "performs no action") {
		t.Fatalf("description does not say what the tool does: %q", def.Description)
	}
	// Annotations are load-bearing: a MISSING annotations object counts as
	// destructive in the pipeline, which would put a HITL prompt in front of
	// reading a document.
	var ann struct {
		ReadOnly    bool `json:"readOnlyHint"`
		Destructive bool `json:"destructiveHint"`
	}
	if err := json.Unmarshal(def.Annotations, &ann); err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if !ann.ReadOnly || ann.Destructive {
		t.Fatalf("annotations = %s, want read-only and non-destructive", def.Annotations)
	}

	if _, err := m.Disable(ctx, sk.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("refresh after disable: %v", err)
	}
	if got := p.ToolNames(); len(got) != 0 {
		t.Fatalf("a disabled skill stayed exposed: %v", got)
	}
}

// TestProviderCallReturnsSkillDocument: the call returns SKILL.md plus the
// manifest of files that are NOT delivered over this path (the honest
// tiering of docs/modules/config.md).
func TestProviderCallReturnsSkillDocument(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	sk, _ := addSample(t, m)
	p := NewProvider(m)
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	res, err := p.Call(ctx, RawToolName(sk.ID), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("call reported an error result: %s", res.Content)
	}
	text := decodeText(t, res.Content)
	for _, want := range []string{"# PDF Tools (1.0.0)", "Use pdftotext.", "ref/notes.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("document is missing %q:\n%s", want, text)
		}
	}
	// Deterministic rendering: the same library produces the same bytes.
	again, err := p.Call(ctx, RawToolName(sk.ID), nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if decodeText(t, again.Content) != text {
		t.Fatal("skill document rendering is not deterministic")
	}
}

// TestProviderCallRevalidatesLiveState is the closed direction of the
// snapshot: Tools() may be stale, Call may not. A skill disabled since the
// last Refresh is refused even though its tool is still listed.
func TestProviderCallRevalidatesLiveState(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t)
	sk, _ := addSample(t, m)
	p := NewProvider(m)
	if err := p.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := m.Disable(ctx, sk.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// Deliberately NOT refreshed: the snapshot still lists the tool.
	if _, ok := p.SkillIDFor(RawToolName(sk.ID)); !ok {
		t.Fatal("test precondition: the stale snapshot should still list the skill")
	}

	res, err := p.Call(ctx, RawToolName(sk.ID), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError || !strings.Contains(decodeText(t, res.Content), "disabled") {
		t.Fatalf("a disabled skill was served from a stale snapshot: %s", res.Content)
	}

	// An unknown tool name is the same shape (an answer the agent can act
	// on), never a transport-level failure.
	if _, err := p.Read(ctx, "skill_does_not_exist"); !errors.Is(err, ErrSkillUnavailable) {
		t.Fatalf("error = %v, want ErrSkillUnavailable", err)
	}
	res, err = p.Call(ctx, "skill_does_not_exist", nil)
	if err != nil || !res.IsError {
		t.Fatalf("unknown tool: res %+v err %v", res, err)
	}
}

// TestRawToolNameSanitizes keeps ids with unusual bytes callable.
func TestRawToolNameSanitizes(t *testing.T) {
	t.Parallel()
	if got := RawToolName("pdf.tools/v2"); got != "skill_pdf_tools_v2" {
		t.Fatalf("RawToolName = %q", got)
	}
}
