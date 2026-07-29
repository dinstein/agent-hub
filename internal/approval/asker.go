package approval

import "context"

// SendFunc carries one approval request to the daemon broker and returns its
// decision. Stage 2 provides the real implementation over the authenticated
// UDS control connection; tests inject fakes.
type SendFunc func(ctx context.Context, req Request) (Decision, error)

// RemoteAsker is the stdio-gateway-side Asker: it forwards Ask over an
// injected transport and degrades to Unreachable on ANY failure — nil or
// unwired transport, transport error, or an out-of-range decision value.
// Fail direction: a gateway that cannot reach the daemon must reject the
// gated call, never wave it through (docs/flows.md, inherited toolport
// broker-unreachable semantics).
type RemoteAsker struct {
	// Send is the control-connection transport. nil = daemon not wired.
	Send SendFunc
}

var _ Asker = (*RemoteAsker)(nil)

// Ask implements Asker.
func (a *RemoteAsker) Ask(ctx context.Context, req Request) Decision {
	if a == nil || a.Send == nil {
		return Unreachable
	}
	d, err := a.Send(ctx, req)
	if err != nil {
		return Unreachable
	}
	if !d.valid() {
		// A corrupted or future-versioned decision must not be trusted.
		return Unreachable
	}
	return d
}
