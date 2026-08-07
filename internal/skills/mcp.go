package skills

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// This file is the skills-over-MCP supply face (docs/modules/config.md, the 5.3
// placeholder): the enabled library exposed to an upstream agent as
// READ-ONLY MCP tools, one per skill, returning that skill's SKILL.md.
//
// It is the honest half of the tiering this package's doc states: file
// materialization stops at CLIENT granularity, so a client that must switch
// skill sets per SESSION — or that never reads the filesystem at all — can
// only be served over the protocol, where agenthub is on the read path.
//
// Three properties are load-bearing and are why this type is a thin
// projection rather than its own subsystem:
//
//  1. TOOL SHAPE, NOT RESOURCE SHAPE — for now. docs/modules/config.md prefers MCP
//     resources; the gateway exposes no resources face yet (its upstream
//     surface is tools-only, gateway doc), and inventing one here would put
//     a protocol surface in a subsystem package. Tools are the honest
//     available shape: same content, same governance, no pretend capability.
//     When the resources face lands, Tools() gets a sibling and the content
//     accessor below is already the shared half.
//
//  2. THE HOST STAYS ON THE GATE PATH. This type never answers a call
//     itself in any privileged way: the assembling gateway routes it through
//     the same pipeline.Execute every downstream call takes, so the scope
//     and token tier gates apply to it exactly as they do to a downstream
//     tool. SKILL.md is a first-class prompt injection carrier (5.1.5), and
//     nothing here scans it: agenthub inspects no content on any path, so a
//     scanner local to this one would be the only policy in the product that
//     reads what it governs.
//
//  3. LIVE ENABLEMENT AT CALL TIME. Tools() serves a snapshot (it is called
//     on every catalog build and must not do I/O), but Call re-reads the
//     library: a skill disabled or removed since the last Refresh is
//     refused, never served from a stale snapshot. The closed direction.

// ProviderID is the pseudo-server id of the skills supply face. It is the
// exposed-name prefix ("skills__…"), the RouteOf provenance of every skill
// call, and the scope key an operator uses to disable the whole face for a
// profile or client.
const ProviderID = "skills"

// toolPrefix prefixes the RAW tool name of one skill: skill_<id>. Kept even
// though the router prefixes the exposed name with the pseudo-server id —
// the raw name is what the call ledger records and what a scope allow list
// keys on, and "skill_" is what makes those readable.
const toolPrefix = "skill_"

// contentLimit bounds one skill document served over MCP. A skill larger
// than this is a bundle, not a prompt; the reply is truncated with a marker
// rather than silently blowing the caller's context budget (result shaping
// downstream will bound it again — this is the cheap first cut).
const contentLimit = 256 << 10

// Provider serves the enabled skill library as read-only MCP tools. It
// satisfies router.Provider (structurally — this package does not import
// router, which would invert the dependency).
type Provider struct {
	mgr *Manager
	// snap is the tool projection, replaced wholesale by Refresh. Reads are
	// lock-free: a catalog build must never block on the skills store.
	snap atomic.Pointer[providerSnapshot]
}

type providerSnapshot struct {
	tools []mcp.ToolDef
	byRaw map[string]string // raw tool name -> skill id
	at    time.Time
}

// NewProvider builds the supply face over a library manager. The snapshot
// starts EMPTY: nothing is exposed until Refresh succeeds, so a broken or
// unreadable store advertises no skills rather than a stale set.
func NewProvider(m *Manager) *Provider {
	p := &Provider{mgr: m}
	p.snap.Store(&providerSnapshot{byRaw: map[string]string{}})
	return p
}

// ID implements the provider contract.
func (p *Provider) ID() string { return ProviderID }

// Refresh rebuilds the tool projection from the library. Call it at
// assembly time and whenever the library may have changed; failures leave
// the previous snapshot in place (serving the last known-good set beats
// serving nothing because a lock was busy).
func (p *Provider) Refresh(ctx context.Context) error {
	views, err := p.mgr.List(ctx, ListOptions{})
	if err != nil {
		return err
	}
	snap := &providerSnapshot{byRaw: make(map[string]string, len(views)), at: time.Now()}
	for _, v := range views {
		sk := v.Skill
		if !sk.Enabled {
			// Disabled skills are INVISIBLE, not listed-and-refused: the
			// same anti-probing rule scope narrowing follows (docs/flows.md).
			continue
		}
		raw := RawToolName(sk.ID)
		if _, dup := snap.byRaw[raw]; dup {
			// Two ids sanitizing to one tool name. Keep the first in sorted
			// order (List is sorted) and skip the rest: a silently shadowed
			// skill is worse than an absent one.
			continue
		}
		snap.byRaw[raw] = sk.ID
		snap.tools = append(snap.tools, mcp.ToolDef{
			Name:        raw,
			Title:       sk.Name,
			Description: describe(sk),
			InputSchema: InputSchema(),
			Annotations: Annotations(),
		})
	}
	slices.SortFunc(snap.tools, func(a, b mcp.ToolDef) int { return cmp.Compare(a.Name, b.Name) })
	p.snap.Store(snap)
	return nil
}

// Tools implements the provider contract: the current snapshot, never I/O.
// The returned slice is a copy; the ToolDef payloads are immutable.
func (p *Provider) Tools() []mcp.ToolDef {
	snap := p.snap.Load()
	out := make([]mcp.ToolDef, len(snap.tools))
	copy(out, snap.tools)
	return out
}

// Call implements the provider contract: read one skill document.
//
// Arguments are ignored on purpose — the schema declares none, and
// rejecting a caller that sent a stray field would fail a read-only lookup
// for a reason that does not matter. An unavailable skill comes back as an
// IS-ERROR RESULT rather than a protocol error, because "this skill is not
// served to you" is an answer the agent can act on, and the two failure
// shapes must not be confusable with a transport failure.
func (p *Provider) Call(ctx context.Context, rawTool string, _ json.RawMessage) (*mcp.CallResult, error) {
	content, err := p.Read(ctx, rawTool)
	if err != nil {
		if errors.Is(err, ErrSkillUnavailable) {
			return textResult(err.Error(), true), nil
		}
		return nil, err
	}
	return textResult(content.Text, false), nil
}

// textResult builds a single-text-item CallResult.
func textResult(text string, isErr bool) *mcp.CallResult {
	raw, err := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	if err != nil {
		// Unreachable: a string always marshals. Stay closed rather than
		// return a half-built result.
		raw = json.RawMessage(`[]`)
	}
	return &mcp.CallResult{Content: raw, IsError: isErr}
}

// ToolNames returns the raw tool names currently exposed, sorted. It exists
// for callers that need the projection without the protocol types (CLI,
// diagnostics, tests).
func (p *Provider) ToolNames() []string {
	snap := p.snap.Load()
	out := make([]string, 0, len(snap.tools))
	for _, t := range snap.tools {
		out = append(out, t.Name)
	}
	return out
}

// SkillIDFor maps a raw tool name back to its skill id.
func (p *Provider) SkillIDFor(rawTool string) (string, bool) {
	id, ok := p.snap.Load().byRaw[rawTool]
	return id, ok
}

// RawToolName is the frozen raw-name projection of a skill id. Sanitizing
// mirrors the router's exposed-name charset so an id with unusual bytes
// still yields a callable tool.
func RawToolName(skillID string) string {
	var b strings.Builder
	b.Grow(len(toolPrefix) + len(skillID))
	b.WriteString(toolPrefix)
	for _, r := range skillID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// describe renders the tool description: the skill's own description plus
// the version, and an explicit statement of what calling it does. Agents
// pick tools from this text, so it says "returns the instructions" rather
// than leaving the reader to guess that a skill tool performs the skill.
func describe(sk Skill) string {
	var b strings.Builder
	if sk.Description != "" {
		b.WriteString(sk.Description)
		b.WriteString(" ")
	}
	fmt.Fprintf(&b, "(agenthub skill %q, version %s — calling this tool returns the skill's instructions; it performs no action.)",
		sk.ID, sk.Version)
	return b.String()
}

// Content is one skill's document as served over MCP: the SKILL.md body
// verbatim plus the bundled-file manifest (docs/modules/config.md read_skill).
type Content struct {
	SkillID string
	Text    string
	// Truncated reports that Text was cut at contentLimit.
	Truncated bool
}

// ErrSkillUnavailable reports a skill that is not (or no longer) served
// over MCP: unknown tool name, removed, or disabled since the last Refresh.
var ErrSkillUnavailable = errors.New("skills: skill not available over MCP")

// Read returns the document behind one raw tool name, re-validating
// enablement against the LIVE library first (see property 3 above).
func (p *Provider) Read(ctx context.Context, rawTool string) (*Content, error) {
	id, ok := p.SkillIDFor(rawTool)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSkillUnavailable, rawTool)
	}
	view, err := p.mgr.Inspect(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrSkillUnavailable, id)
		}
		return nil, err
	}
	sk := view.Skill
	if !sk.Enabled {
		return nil, fmt.Errorf("%w: %q is disabled", ErrSkillUnavailable, id)
	}

	body, err := p.body(&sk)
	if err != nil {
		return nil, err
	}
	text, truncated := renderSkillDocument(&sk, body)
	return &Content{SkillID: id, Text: text, Truncated: truncated}, nil
}

// body reads SKILL.md from the library copy. A missing file yields an empty
// body (the manifest below still describes the package) rather than an
// error: a skill whose document was lost is degraded, not fatal.
func (p *Provider) body(sk *Skill) (string, error) {
	raw, err := os.ReadFile(filepath.Join(p.mgr.SkillPath(sk), SkillFileName))
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	default:
		return "", err
	}
	meta, perr := ParseSkillMD(raw)
	if perr != nil {
		// Unparsable frontmatter: serve the raw bytes rather than nothing.
		// The content is what the agent needs; the manifest is metadata.
		return strings.TrimRight(string(raw), "\n"), nil
	}
	return strings.TrimRight(meta.Body, "\n"), nil
}

// renderSkillDocument is the frozen, deterministic rendering of one skill
// for the MCP face: heading, description, body, then the sorted list of
// bundled files that are NOT materialized by this path.
//
// Naming the unmaterialized attachments is the same honesty rule the
// sentinel-block renderer follows: a protocol reply cannot hand over a
// directory, and silently omitting the files would make the agent believe
// it has the whole package.
func renderSkillDocument(sk *Skill, body string) (string, bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%s)\n", sk.Name, sk.Version)
	if sk.Description != "" {
		b.WriteString("\n" + sk.Description + "\n")
	}
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}
	attach := unmaterializedFiles(sk)
	if len(attach) > 0 {
		fmt.Fprintf(&b, "\n_Bundled files kept in the agenthub library, not delivered over MCP: %s_\n",
			strings.Join(attach, ", "))
	}
	out := b.String()
	if len(out) <= contentLimit {
		return out, false
	}
	return out[:contentLimit] + "\n\n_[truncated by agenthub: skill document exceeds the MCP supply limit]_\n", true
}

// InputSchema is the frozen argument schema of every skill tool: no
// arguments. Reading a skill takes none, and an open schema would invite an
// agent to smuggle parameters into a read-only surface.
func InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// Annotations is the frozen annotation object of every skill tool. It is
// load-bearing, not decoration: the pipeline's destructive predicate treats
// MISSING annotations as destructive (fail-closed), so an unannotated
// read-only tool would raise an approval prompt on every call.
func Annotations() json.RawMessage {
	return json.RawMessage(`{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}`)
}
