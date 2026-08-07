package gateway

import (
	"encoding/json"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// The 2026-07-28 subscription state of one upstream session.
//
// On the HTTP face the subscription IS the response body, and
// internal/httpbridge owns it: the stream stays open and carries what the
// client asked for. The stdio face has no body to hold open — notifications
// have always gone inline on stdout — so what a subscription changes here is
// not HOW a notification travels but WHETHER it is sent at all.
//
// Having subscribed is the whole rule, and the protocol generation is NOT:
//
//   - A session that subscribed receives exactly what it asked for. That is
//     2026-07-28's MUST — "the server MUST NOT send a type absent from the
//     filter" — and it is honoured for anyone who uses the mechanism.
//   - A session that never subscribed keeps receiving notifications inline,
//     exactly as before, whatever generation it negotiated.
//
// **The second bullet is a deliberate deviation, and it is worth being
// explicit about.** A conformant 2026-07-28 server sends nothing a client did
// not subscribe to, so a 2026 stdio client that never calls this method
// receives a notification it did not ask for. The alternative was tried
// first and rejected: withholding it means a client that does not use
// subscriptions/listen holds a tool set that can go stale forever, with
// nothing saying so — which is the exact failure the HTTP face grew a stream
// to remove, reintroduced on the other face.
//
// The symmetry argument for withholding does not survive inspection either.
// On the HTTP face "no subscription, no notification" is not a policy, it is
// the absence of a channel: there is no open body to write into. On stdio
// the channel is always there. Sending an edge nobody asked for costs a
// client that ignores it nothing; withholding one costs a client that wanted
// it everything, and neither client can tell that it happened.

// producedNotifications is what this gateway ever sends UPSTREAM.
//
// One method, and the list exists so the honoured filter can be intersected
// with reality rather than with the protocol's full vocabulary — a client
// told it is subscribed to prompts/list_changed would wait forever, since
// nothing here produces one. internal/httpbridge keeps the same set for the
// same reason and test/buildrules holds the three of them together
// (TestCarriedNotificationsMatchTheGateway).
var producedNotifications = map[string]bool{
	mcp.NotificationToolsListChanged: true,
}

// handleSubscriptionsListen answers the 2026-07-28 subscription request on
// the stdio face.
//
// **This binding answers immediately, and that is a decision rather than a
// reading.** The specification defines this method's response for streamable
// HTTP, where it is the long-lived SSE body and no JSON-RPC response is ever
// sent. stdio has no such body, which leaves two options, and neither is
// written down: answer, or leave the request open forever. Answering wins
// because the failure modes are not symmetric — a client that expects a
// response and never gets one HANGS, while a client that expects a stream
// and gets a result reads it as "subscribed", which is what it means here.
// Notifications continue to arrive inline afterwards, so nothing about the
// subscription ends when the response does.
//
// A ≤ 2025-11-25 session that sends this method is answered the same way.
// It gained nothing — that generation already receives these notifications —
// but refusing a method by protocol generation would be a second version
// gate for no benefit.
func (g *gateway) handleSubscriptionsListen(req *mcp.Request) {
	var params mcp.SubscriptionsListenParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInvalidParams,
				Message: "subscriptions/listen params could not be read",
			}))
			return
		}
	}
	honoured := honouredFilter(params.Notifications)

	g.mu.Lock()
	g.subscribed = &honoured
	g.mu.Unlock()

	// The acknowledgement goes first, as it does on the HTTP face: it is the
	// only place a client learns that a type it asked for will never arrive.
	ack, err := json.Marshal(mcp.SubscriptionsAcknowledgedParams{
		Notifications: honoured,
		Meta:          &mcp.NotificationMeta{SubscriptionID: req.ID},
	})
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInternalError, Message: "the subscription could not be acknowledged",
		}))
		return
	}
	g.reply(mcp.NewNotification(mcp.NotificationSubscriptionsAcknowledged, ack))

	result, err := json.Marshal(struct {
		ResultType string `json:"resultType"`
	}{ResultType: mcp.ResultTypeComplete})
	if err != nil {
		g.reply(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code: mcp.CodeInternalError, Message: "the subscription result could not be encoded",
		}))
		return
	}
	g.reply(mcp.NewResponse(req.ID, result))
}

// honouredFilter intersects what a client asked for with what this gateway
// produces. ResourceSubscriptions stays nil: this hub subscribes to no
// individual resource on a client's behalf, and nil is "none" where [] would
// be "an empty set of them".
func honouredFilter(req mcp.SubscriptionFilter) mcp.SubscriptionFilter {
	return mcp.SubscriptionFilter{
		ToolsListChanged:     req.ToolsListChanged && producedNotifications[mcp.NotificationToolsListChanged],
		PromptsListChanged:   req.PromptsListChanged && producedNotifications[mcp.NotificationPromptsListChanged],
		ResourcesListChanged: req.ResourcesListChanged && producedNotifications[mcp.NotificationResourcesListChanged],
	}
}

// mayNotify reports whether one notification method may be sent to this
// session.
//
// Failure direction: a session that has not narrowed gets EVERYTHING. The
// filter only ever subtracts, and only for a client that asked for it — so
// the way this can be wrong is by sending an edge nobody wanted, never by
// swallowing one somebody was waiting for. See the file comment for why that
// direction was chosen over conformance.
func (g *gateway) mayNotify(method string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.subscribed == nil {
		return true
	}
	switch method {
	case mcp.NotificationToolsListChanged:
		return g.subscribed.ToolsListChanged
	case mcp.NotificationPromptsListChanged:
		return g.subscribed.PromptsListChanged
	case mcp.NotificationResourcesListChanged:
		return g.subscribed.ResourcesListChanged
	default:
		// An allow list: a method this switch does not name is not one the
		// client subscribed to, whatever it is.
		return false
	}
}
