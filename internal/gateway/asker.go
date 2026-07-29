package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// This file is the gateway side of the HITL approval link (docs/flows.md):
// the pipeline's HITL gate calls Ask, which forwards the gated call over the
// daemon control socket (POST /v1/approvals/ask) and blocks until a human —
// on ANY frontend — decides. Fail-closed contract inherited wholesale: a
// missing control link, an unreachable daemon, a malformed response, an
// unknown decision string — every failure mode maps to a rejection, never
// to execution. Argument bytes travel only over the authenticated UDS and
// are never written to disk on either side.

// callMetaKey carries per-call context from handleToolsCall to the asker:
// the raw argument bytes (for frontend display) and the routed tool's
// identity-bearing definition (for the live fingerprint). The pipeline
// deliberately does not know about either — it passes ctx through.
type callMetaKey struct{}

type callMeta struct {
	args json.RawMessage
	snap integrity.ToolSnapshot
}

// withCallMeta stamps the per-call approval metadata into ctx.
func withCallMeta(ctx context.Context, meta callMeta) context.Context {
	return context.WithValue(ctx, callMetaKey{}, meta)
}

func callMetaFrom(ctx context.Context) (callMeta, bool) {
	m, ok := ctx.Value(callMetaKey{}).(callMeta)
	return m, ok
}

// gwAsker implements pipeline.Asker over the daemon control socket.
type gwAsker struct {
	g *gateway
}

var _ pipeline.Asker = (*gwAsker)(nil)

// Ask forwards one gated call to the daemon broker and blocks until its
// decision. It never returns an error: every failure is folded into a
// blocking Decision so the HITL gate's fail-closed switch handles all of
// them uniformly.
func (a *gwAsker) Ask(ctx context.Context, req pipeline.ApprovalRequest) (pipeline.Decision, error) {
	remote := &approval.RemoteAsker{Send: a.send}
	wire := approval.Request{
		Server:     req.ServerID,
		Tool:       req.RawTool,
		ArgsHash:   req.ArgsHash,
		GateReason: approval.ReasonPolicy,
	}
	if req.Destructive {
		wire.GateReason = approval.ReasonDestructive
	}
	if meta, ok := callMetaFrom(ctx); ok {
		wire.ArgsJSON = meta.args
		// Fingerprint of the LIVE definition (allowlist key). A definition
		// that cannot be fingerprinted yields "" — which never matches any
		// remembered grant, so the call still goes to a human (fail-closed).
		if fp, err := integrity.Fingerprint(meta.snap); err == nil {
			wire.Fingerprint = fp
		} else {
			a.g.log.Warn("tool not fingerprintable; approval cannot be remembered",
				"server", req.ServerID, "tool", req.RawTool, "error", err)
		}
	}
	wire.Client = a.g.cfg.ClientID
	if a.g.ctl != nil {
		wire.SessionID = a.g.ctl.Session()
	}

	// RemoteAsker degrades EVERY transport/format failure to Unreachable.
	switch remote.Ask(ctx, wire) {
	case approval.Approved:
		return pipeline.DecisionApproved, nil
	case approval.Denied:
		return pipeline.DecisionDenied, nil
	case approval.Timedout:
		return pipeline.DecisionTimeout, nil
	case approval.Stale:
		// Distinct from a dead broker in logs/audit, equally blocking.
		return pipeline.Decision("stale"), nil
	default: // Unreachable and anything unknown
		return pipeline.DecisionUnavailable, nil
	}
}

// send is the approval.SendFunc transport: one blocking POST over the ctl
// socket. Errors surface to RemoteAsker, which maps them to Unreachable.
func (a *gwAsker) send(ctx context.Context, req approval.Request) (approval.Decision, error) {
	l := a.g.ctl
	if l == nil {
		return approval.Unreachable, errors.New("gateway: no daemon control link")
	}
	// Bind the ask to the current daemon connection: an Ask outlives any
	// deadline by design (it waits for a human), so the only thing that may
	// end it early is the broker itself going away. Without a live link
	// there is nobody to decide — fail closed immediately.
	ctx, cancel := mergeCancel(ctx, l.alive())
	defer cancel()
	ask := ctlapi.ApprovalAskWire{
		Server:      req.Server,
		Tool:        req.Tool,
		Args:        req.ArgsJSON,
		ArgsHash:    req.ArgsHash,
		Fingerprint: req.Fingerprint,
		GateReason:  string(req.GateReason),
		Client:      req.Client,
		SessionID:   req.SessionID,
	}
	var res ctlapi.ApprovalDecisionWire
	if err := l.post(ctx, "/v1/approvals/ask", "gateway:"+a.g.cfg.ClientID, ask, &res); err != nil {
		return approval.Unreachable, err
	}
	d, ok := approval.ParseDecision(res.Decision)
	if !ok {
		// A decision string this binary does not know must not be trusted.
		return approval.Unreachable, errors.New("gateway: unknown approval decision " + res.Decision)
	}
	return d, nil
}

// mergeCancel returns a context cancelled when either parent is: the
// caller's ctx (upstream cancellation) or the link-liveness ctx (the broker
// went away). context.WithoutCancel-style plumbing is deliberately avoided
// here — an approval must not survive the connection that carries it.
func mergeCancel(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-ctx.Done():
		case <-stop:
		}
	}()
	return ctx, func() { close(stop); cancel() }
}
