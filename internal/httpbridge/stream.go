package httpbridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// This file is the server→client direction: streamable HTTP's own
// notification channel, which agenthub exposes because nothing else can tell
// a client over this face that its tool set moved. Catalog refresh is driven
// entirely by tools/list_changed — no poll, no TTL, no re-list except on a
// reconnect a healthy client never performs — so a face without this channel
// serves a stale tool set and says nothing, while its own capability
// declaration promises otherwise.
//
// What this is NOT is a second SSE transport. Ruling #29 is about the
// 2024-11-05 HTTP+SSE binding — two endpoints, an `endpoint` event naming
// where to POST — and that remains read-side only. What is written here is
// the response body shape streamable HTTP has defined for a GET since
// 2025-03-26.

const (
	// mediaSSE is the one content type a notification stream answers with.
	mediaSSE = "text/event-stream"
	// streamKeepAlive is how often an idle stream writes a comment line.
	//
	// Not decoration: an idle TCP connection through a proxy, a load
	// balancer or a laptop's NAT is reaped on a timer nobody here controls,
	// and a stream that carries a notification once a day would be dead
	// long before its first one. The comment is also the only way this side
	// LEARNS the peer is gone — a write to a closed connection is the error
	// that ends the loop, and without traffic there is no write.
	//
	// 25s is under the common 30s idle timeout with room for one lost tick.
	streamKeepAlive = 25 * time.Second
)

// handleGet answers the ≤ 2025-11-25 notification stream.
//
// The specification's shape: the client GETs the MCP endpoint with
// `Accept: text/event-stream`, and the server either opens the stream or
// answers 405 to say it offers none. A GET that did not ask for the stream
// gets that 405 unchanged — it is the accurate answer to "I want something
// else from this endpoint", which is nothing.
//
// The session is REQUIRED here, and that is what confines this handler to
// one generation: ≤ 2025-11-25 minted a session at initialize, while
// 2026-07-28 removed the header entirely and replaced this stream with
// subscriptions/listen. A 2026 client arriving here has no session id to
// present and is told so rather than quietly served the older shape.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, c *Caller, held *slot) {
	if !acceptsSSE(r) {
		s.fail(w, r, errNoStream)
		return
	}
	sess, ok := s.resolveSession(w, r, c, true)
	if !ok {
		return
	}
	// nil filter: this generation has no way to ask for a subset, so the
	// client gets whatever the gateway produces for its credential.
	s.serveStream(w, r, c, sess, nil, mcp.ID{}, held)
}

// serveStream opens one notification stream and writes it until the client
// leaves, the subscription ends, or a write fails.
//
// subID is echoed in each event's _meta when set (the 2026-07-28
// correlation id); the ≤ 2025-11-25 stream passes the zero ID and carries no
// _meta at all, because that generation defines none here.
func (s *Server) serveStream(
	w http.ResponseWriter, r *http.Request, c *Caller, sess *Session,
	accept func(method string) bool, subID mcp.ID, held *slot,
) {
	// Order matters: take the stream slot BEFORE giving back the in-flight
	// one. Released first, a burst of stream opens would each be counted
	// against neither quota for the instant in between.
	if !s.streams.acquire() {
		s.fail(w, r, errNoStreams)
		return
	}
	defer s.streams.release()
	// This request has stopped being one. Everything below is waiting, and
	// waiting must not consume the ceiling that bounds work.
	held.release()

	sub, err := s.dispatcher.Subscribe(r.Context(), c, sess, accept)
	if err != nil || sub == nil {
		// No detail: the message crosses an authenticated but untrusted
		// boundary, and the reason a gateway would not assemble is not the
		// client's business. The log carries it.
		s.log.Warn("notification stream could not be opened",
			"caller", string(c.Kind), "token", c.Token, "error", err)
		s.fail(w, r, errStreamSetup)
		return
	}
	defer sub.Close()

	// No write deadline is set anywhere on this path, and none may be: the
	// head-side limits are the only ones this face applies, and a write
	// timeout would kill exactly the connection this handler exists to keep.
	w.Header().Set("Content-Type", mediaSSE)
	w.Header().Set("Cache-Control", "no-store")
	// A proxy that buffers turns every notification into "eventually", which
	// for a list-changed edge is indistinguishable from never.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// A transport with no flush support cannot carry this: everything would
	// sit in a buffer until the handler returned, which for a stream is
	// forever. Fail rather than serve a stream that delivers nothing — the
	// head is already written, so the client sees the connection close.
	if err := rc.Flush(); err != nil {
		s.log.Warn("notification stream not flushable; closing", "error", err)
		return
	}

	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	s.log.Debug("notification stream opened", logx.Session(sessionID), "caller", string(c.Kind))
	defer s.log.Debug("notification stream closed", logx.Session(sessionID))

	ticker := time.NewTicker(streamKeepAlive)
	defer ticker.Stop()

	// One goroutine parks in Next while this one selects, because Next is
	// the only blocking call and the keep-alive has to fire while it blocks.
	notes := make(chan *mcp.Notification)
	go func() {
		defer close(notes)
		for {
			n, ok := sub.Next(r.Context())
			if !ok {
				return
			}
			select {
			case notes <- n:
			case <-r.Context().Done():
				return
			}
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case n, ok := <-notes:
			if !ok {
				return // the subscription ended; the gateway is going away
			}
			if err := writeSSEMessage(w, n, subID); err != nil {
				s.log.Debug("notification not delivered; stream closing", "error", err)
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			// A delivered notification is a live client, so the session it
			// belongs to is not idle. Without this a stream outlives its own
			// session: the TTL only advances on requests, and a client that
			// is being pushed to sends none.
			s.sessions.touch(sessionID)
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			s.sessions.touch(sessionID)
		}
	}
}

// writeSSEMessage writes one notification as an SSE `message` event.
//
// The event name is `message` because that is the one every MCP client reads;
// this side's own read half (internal/mcp/transport) skips any other name as
// a vendor event, and so do the SDKs.
func writeSSEMessage(w http.ResponseWriter, n *mcp.Notification, subID mcp.ID) error {
	out := n
	if subID.IsSet() {
		// Copy rather than mutate: one notification is fanned out to every
		// subscriber of this credential, and each carries its own id.
		withMeta := *n
		params, err := injectSubscriptionID(n.Params, subID)
		if err != nil {
			return err
		}
		withMeta.Params = params
		out = &withMeta
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	// SSE frames on newlines, so a payload containing one would end the
	// event early. JSON encoding cannot produce a bare newline inside a
	// value, and the marshalled message is a single line — but the check is
	// cheap and the failure it prevents is a corrupted stream rather than a
	// dropped message.
	if bytesContainNewline(data) {
		return fmt.Errorf("httpbridge: refusing to write a multi-line SSE payload for %q", n.Method)
	}
	_, err = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	return err
}

func bytesContainNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

// injectSubscriptionID adds the 2026-07-28 correlation id to a notification's
// params without disturbing anything else in them.
//
// The key comes from mcp.NotificationMeta's own json tag rather than a string
// written here: the reserved io.modelcontextprotocol/* namespace has one
// declaration in this tree, and a second spelling of a key is a bug that
// only shows up against a real client.
func injectSubscriptionID(params json.RawMessage, subID mcp.ID) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &fields); err != nil {
			return nil, err
		}
	}
	meta, err := json.Marshal(mcp.NotificationMeta{SubscriptionID: subID})
	if err != nil {
		return nil, err
	}
	// A notification the gateway produced carries no _meta of its own — it
	// is built by this process, not forwarded — so there is nothing here to
	// merge with. Overwriting rather than merging keeps that assumption
	// visible: if one ever does arrive with _meta, this is the line that
	// has to change.
	fields["_meta"] = meta
	return json.Marshal(fields)
}

// acceptsSSE reports whether the client will take a text/event-stream answer.
//
// Unlike acceptsJSON, an ABSENT Accept header is false here. The two defaults
// differ because the questions do: a POST with no Accept is ordinary HTTP
// tooling that will read whatever comes back, while a GET with no Accept is
// not asking for a stream, and answering one would hand a long-lived response
// to a client that expected a document and will hang waiting for its end.
func acceptsSSE(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "*/*", "text/*", mediaSSE:
			return true
		}
	}
	return false
}
