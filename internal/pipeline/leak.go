package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/guard/leakguard"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// This file is the leakguard half of defend_and_shape (docs/modules/security.md). Where
// the injection scan defends the way IN, this defends the way OUT: a tool
// result carrying a private key, an API token or a card number must not reach
// the agent unnoticed.
//
// Stage order inside defend_and_shape is load-bearing:
//
//	injection → leakguard → shaping
//
// injection first because a blocked result has no payload left to leak;
// leakguard before shaping because scanning must see the WHOLE result, and
// because the shaping cache must never hold an unredacted secret (7.6: "the
// cache never holds an unredacted secret") — shaping runs on the
// already-redacted text.

// LeakEvent is one leakguard report. It carries redacted records ONLY: rule,
// severity, position, length. No content, no preview, no excerpt — the audit
// trail must not become a second copy of the leak.
type LeakEvent struct {
	// Exposed / ServerID / RawTool identify the call.
	Exposed  string
	ServerID string
	RawTool  string
	// Records are the findings, content-free by construction.
	Records []leakguard.AuditRecord
	// Redacted counts spans replaced in the delivered result (0 in audit
	// mode, which never rewrites).
	Redacted int
	// Truncated reports that more findings existed than the scanner's cap
	// reports.
	Truncated bool
}

// LeakFunc receives one LeakEvent. Unlike SelfHealFunc it does NOT run on the
// call path: the pipeline dispatches it on its own goroutine with a
// cancellation-free context, so a slow audit writer can never delay a tool
// result — and, in audit mode, so the scan itself costs the call nothing
// (ruling #17: the audit hook is default-on precisely because it is free).
type LeakFunc func(ctx context.Context, ev LeakEvent)

// guardLeaks is the leakguard stage. It runs after the injection verdict and
// before shaping, over both branches (a hostile server must not smuggle a
// secret out inside an error message any more than it can dodge the injection
// scan that way).
//
// Failure direction: detection fails open (an unmatched secret is delivered),
// disposition fails closed (in inline mode a payload that cannot be rewritten
// is withheld rather than delivered unredacted — see redactResult).
func (s *defendAndShape) guardLeaks(ctx context.Context, req *CallRequest, res *mcp.CallResult, callErr error) (*mcp.CallResult, error) {
	if s.leakScanner == nil {
		return res, callErr // leak scanning not wired (M0/M1-compat assembly)
	}
	var pol leakguard.Policy
	if s.leakPolicy != nil {
		pol = s.leakPolicy()
	}
	if pol.Mode == leakguard.ModeOff {
		return res, callErr
	}
	if pol.Mode != leakguard.ModeInline && s.onLeak == nil {
		return res, callErr // audit with no consumer: nothing to compute
	}
	segments, slots := leakSegments(res, callErr)
	if len(segments) == 0 {
		return res, callErr
	}
	if pol.Mode != leakguard.ModeInline {
		// AUDIT: scan off the call path entirely. The segments are strings —
		// immutable — so handing them to another goroutine cannot race with
		// the result travelling back to the client.
		s.auditLeaksAsync(ctx, req, pol, segments)
		return res, callErr
	}
	sr := s.leakScanner.ScanResult(pol, req.ServerID, segments)
	s.reportLeaks(ctx, req, sr)
	if sr.Action != leakguard.ActionRedact {
		return res, callErr
	}
	return redactResult(res, callErr, segments, sr, slots)
}

// auditLeaksAsync scans and reports on a side goroutine. With no hook wired
// there is nothing to report, so the scan is skipped entirely rather than
// burned for a result nobody reads.
func (s *defendAndShape) auditLeaksAsync(ctx context.Context, req *CallRequest, pol leakguard.Policy, segments []string) {
	if s.onLeak == nil {
		return
	}
	scanner, hook := s.leakScanner, s.onLeak
	ev := LeakEvent{Exposed: req.Exposed, ServerID: req.ServerID, RawTool: req.RawTool}
	serverID := req.ServerID
	// WithoutCancel: the audit of a call must survive the call's own context
	// being cancelled the instant the result is delivered.
	ctx = context.WithoutCancel(ctx)
	go func() {
		sr := scanner.ScanResult(pol, serverID, segments)
		if len(sr.Findings) == 0 {
			return
		}
		ev.Records = leakguard.Records(sr.Findings)
		ev.Truncated = sr.Truncated
		hook(ctx, ev)
	}()
}

// reportLeaks dispatches the inline-mode report (the scan already happened on
// the call path because the rewrite needed it).
func (s *defendAndShape) reportLeaks(ctx context.Context, req *CallRequest, sr leakguard.Result) {
	if s.onLeak == nil || len(sr.Findings) == 0 {
		return
	}
	ev := LeakEvent{
		Exposed:   req.Exposed,
		ServerID:  req.ServerID,
		RawTool:   req.RawTool,
		Records:   leakguard.Records(sr.Findings),
		Redacted:  sr.Redacted,
		Truncated: sr.Truncated,
	}
	hook := s.onLeak
	ctx = context.WithoutCancel(ctx)
	go hook(ctx, ev)
}

// leakSlotKind says where a scanned segment came from, so a redacted segment
// goes back exactly where it was read.
type leakSlotKind int

const (
	slotTextBlock  leakSlotKind = iota // res.Content[idx]["text"]
	slotRawContent                     // res.Content as a whole (unparsable)
	slotStructured                     // res.StructuredContent
	slotError                          // callErr.Error()
)

type leakSlot struct {
	kind leakSlotKind
	idx  int
}

// leakSegments extracts the scannable text together with its provenance. It
// mirrors scanSegments (the injection extractor) and differs in one way: it
// remembers WHERE each segment came from, because leakguard may have to write
// the segment back.
//
// Non-text content blocks (images, audio, embedded resources) are not
// scanned: their base64 payloads would drown the entropy heuristic in noise
// while yielding nothing the rule table can identify. That is a deliberate
// fail-open gap, documented here so it is a decision and not an oversight.
func leakSegments(res *mcp.CallResult, callErr error) ([]string, []leakSlot) {
	if callErr != nil {
		return []string{callErr.Error()}, []leakSlot{{kind: slotError}}
	}
	if res == nil {
		return nil, nil
	}
	var (
		segments []string
		slots    []leakSlot
	)
	if len(res.Content) > 0 {
		var blocks []json.RawMessage
		if err := json.Unmarshal(res.Content, &blocks); err != nil {
			segments = append(segments, string(res.Content))
			slots = append(slots, leakSlot{kind: slotRawContent})
		} else {
			for i, b := range blocks {
				if text, ok := blockText(b); ok && text != "" {
					segments = append(segments, text)
					slots = append(slots, leakSlot{kind: slotTextBlock, idx: i})
				}
			}
		}
	}
	if len(res.StructuredContent) > 0 {
		segments = append(segments, string(res.StructuredContent))
		slots = append(slots, leakSlot{kind: slotStructured})
	}
	return segments, slots
}

// blockText returns the "text" field of one content block when it is a JSON
// string.
func blockText(block json.RawMessage) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(block, &fields); err != nil {
		return "", false
	}
	raw, ok := fields["text"]
	if !ok {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

// redactResult writes the redacted segments back into the outgoing result.
//
// Fail-closed on every path it cannot rewrite cleanly: an unparsable content
// payload is REPLACED by a notice rather than delivered with its secret
// intact, and a structuredContent whose redaction no longer parses as JSON is
// dropped. Delivering the original would defeat the entire stage — inline
// mode was chosen explicitly, so "I could not rewrite it" must not silently
// degrade to "I sent it anyway".
func redactResult(res *mcp.CallResult, callErr error, orig []string, sr leakguard.Result, slots []leakSlot) (*mcp.CallResult, error) {
	ids := leakRuleIDs(sr.Findings)
	// changed(i) reports that segment i actually lost something. Segments the
	// scanner left alone are put back byte for byte — a stage that rewrites
	// what it did not redact would churn every result that merely travelled
	// past it.
	changed := func(i int) bool {
		return i < len(sr.Segments) && i < len(orig) && sr.Segments[i] != orig[i]
	}
	if callErr != nil {
		if !changed(0) {
			return res, callErr
		}
		// The rendered message is redacted; the ORIGINAL error stays in the
		// chain so errors.Is/As — and with them the downstream error-code
		// passthrough — keep working.
		return res, &redactedError{msg: sr.Segments[0], err: callErr}
	}
	if res == nil {
		return res, callErr
	}
	out := *res
	var blocks []json.RawMessage
	if len(out.Content) > 0 {
		if err := json.Unmarshal(out.Content, &blocks); err != nil {
			blocks = nil
		}
	}
	rawWithheld, touched := false, false
	for i, slot := range slots {
		if !changed(i) {
			continue
		}
		touched = true
		text := sr.Segments[i]
		switch slot.kind {
		case slotTextBlock:
			if blocks == nil || slot.idx >= len(blocks) {
				continue
			}
			if replaced, ok := withBlockText(blocks[slot.idx], text); ok {
				blocks[slot.idx] = replaced
			} else {
				// Cannot rewrite this block: replace it wholesale rather than
				// ship it (stay closed).
				blocks[slot.idx] = textBlock(leakBlockNotice(ids))
			}
		case slotRawContent:
			rawWithheld = true
		case slotStructured:
			redacted := json.RawMessage(text)
			if !json.Valid(redacted) {
				out.StructuredContent = nil // drop rather than deliver
				continue
			}
			out.StructuredContent = redacted
		case slotError:
			// unreachable: the error branch returned above.
		}
	}
	if !touched {
		return res, callErr
	}
	switch {
	case rawWithheld:
		out.Content = mustBlocks(textBlock(leakBlockNotice(ids)))
	case blocks != nil:
		out.Content = mustBlocks(append([]json.RawMessage{textBlock(leakNotice(sr, ids))}, blocks...)...)
	default:
		// Nothing to prepend into (structuredContent-only result): the notice
		// becomes the content, so the agent still learns something was cut.
		out.Content = mustBlocks(textBlock(leakNotice(sr, ids)))
	}
	return &out, nil
}

// leakNotice is the block prepended to a redacted result.
func leakNotice(sr leakguard.Result, ids []string) string {
	return fmt.Sprintf(
		"[agenthub leak guard] %d sensitive value(s) redacted from this tool result (%s). "+
			"The redacted spans are marked [REDACTED:<rule>]; ask the operator if you need the raw value.",
		sr.Redacted, ruleList(ids))
}

// leakBlockNotice is what replaces a payload that could not be rewritten.
func leakBlockNotice(ids []string) string {
	return fmt.Sprintf(
		"[agenthub leak guard] this tool result was withheld: sensitive data (%s) was detected in a "+
			"payload that could not be rewritten safely.", ruleList(ids))
}

// withBlockText returns block with its "text" field replaced.
func withBlockText(block json.RawMessage, text string) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(block, &fields); err != nil {
		return nil, false
	}
	raw, err := json.Marshal(text)
	if err != nil {
		return nil, false
	}
	fields["text"] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return out, true
}

// mustBlocks marshals a content array, falling back to an empty array. The
// fallback direction is deliberate: losing the result is acceptable, shipping
// an unredacted one is not.
func mustBlocks(blocks ...json.RawMessage) json.RawMessage {
	out, err := json.Marshal(blocks)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return out
}

// leakRuleIDs returns the distinct rule IDs of findings, in first-seen order
// (ruleList sorts them).
func leakRuleIDs(findings []leakguard.Finding) []string {
	seen := make(map[string]bool, len(findings))
	var ids []string
	for _, f := range findings {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			ids = append(ids, f.Rule)
		}
	}
	return ids
}

// redactedError renders a redacted message while keeping the original error
// in the chain. Unwrap is the load-bearing half: the typed transport/JSON-RPC
// error must stay reachable through errors.Is/As, or redaction would silently
// destroy the downstream error-code passthrough.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
