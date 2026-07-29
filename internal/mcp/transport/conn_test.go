package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// fakePeer is the far (server) end of an in-memory conn: it reads frames
// the client wrote and can write raw bytes back.
type fakePeer struct {
	t  *testing.T
	fr *mcp.FrameReader
	w  io.WriteCloser
}

// newPipeConn wires a conn to a fakePeer over io.Pipes. maxFrame bounds the
// client's reader (use mcp.MaxFrameSize for the real limit).
func newPipeConn(t *testing.T, maxFrame int) (*conn, *fakePeer) {
	t.Helper()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	c := newConn(clientWrite, clientRead, maxFrame)
	c.start()
	p := &fakePeer{t: t, fr: mcp.NewFrameReader(serverRead), w: serverWrite}
	t.Cleanup(func() {
		_ = c.Close()
		_ = serverWrite.Close()
		_ = serverRead.Close()
	})
	return c, p
}

// next returns the next frame the client sent, parsed.
func (p *fakePeer) next() any {
	p.t.Helper()
	line, err := p.fr.Next()
	if err != nil {
		p.t.Fatalf("peer read: %v", err)
	}
	msg, err := mcp.ParseMessage(line)
	if err != nil {
		p.t.Fatalf("peer parse: %v", err)
	}
	return msg
}

func (p *fakePeer) nextRequest() *mcp.Request {
	p.t.Helper()
	req, ok := p.next().(*mcp.Request)
	if !ok {
		p.t.Fatal("expected request")
	}
	return req
}

// writeRaw writes raw bytes to the client (already newline-terminated by
// the caller when needed). Best-effort: when the client poisons its reader
// mid-frame (oversized-frame cases) the tail of the write fails on the
// closed pipe, which is expected.
func (p *fakePeer) writeRaw(data string) {
	_, _ = io.WriteString(p.w, data)
}

func (p *fakePeer) writeFrame(msg any) {
	p.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatal(err)
	}
	p.writeRaw(string(b) + "\n")
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCallResponse(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := p.nextRequest()
		if req.Method != mcp.MethodPing {
			p.t.Errorf("method %q", req.Method)
		}
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`{"ok":true}`)))
	}()
	raw, err := c.Call(testCtx(t), mcp.MethodPing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("result %s", raw)
	}
	<-done
}

func TestCallJSONRPCErrorIsClassFatal(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeMethodNotFound, Message: "nope"}))
	}()
	_, err := c.Call(testCtx(t), "no/such", nil)
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("err = %v, want *Error ClassFatal", err)
	}
	var je *mcp.Error
	if !errors.As(err, &je) || je.Code != mcp.CodeMethodNotFound {
		t.Fatalf("err = %v, want wrapped *mcp.Error -32601", err)
	}
	// An ordinary error response must not poison the connection.
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`1`)))
	}()
	if _, err := c.Call(testCtx(t), mcp.MethodPing, nil); err != nil {
		t.Fatalf("conn unusable after error response: %v", err)
	}
}

// Table-driven: protocol-level poison frames from the server. Each case
// must (a) fail the pending call with ClassUnavailable and the given
// sentinel, (b) leave the conn terminally failed, (c) never panic.
func TestPoisonFrames(t *testing.T) {
	huge := strings.Repeat("x", 4096) + "\n" // exceeds maxFrame=1024 below
	tests := []struct {
		name     string
		frame    string
		sentinel error
	}{
		{name: "invalid json", frame: "{not json}\n", sentinel: mcp.ErrMalformedFrame},
		{name: "wrong jsonrpc version", frame: `{"jsonrpc":"1.0","id":1,"result":{}}` + "\n", sentinel: mcp.ErrMalformedFrame},
		{name: "bool id", frame: `{"jsonrpc":"2.0","id":true,"method":"x"}` + "\n", sentinel: mcp.ErrMalformedFrame},
		{name: "neither request nor response", frame: `{"jsonrpc":"2.0"}` + "\n", sentinel: mcp.ErrMalformedFrame},
		{name: "oversized frame", frame: huge, sentinel: mcp.ErrFrameTooLarge},
		{name: "eof mid stream", frame: "", sentinel: io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p := newPipeConn(t, 1024)
			go func() {
				_ = p.nextRequest()
				if tt.frame == "" {
					_ = p.w.Close() // stdout closes → EOF
					return
				}
				p.writeRaw(tt.frame)
			}()
			_, err := c.Call(testCtx(t), mcp.MethodPing, nil)
			var te *Error
			if !errors.As(err, &te) {
				t.Fatalf("err = %v (%T), want *transport.Error", err, err)
			}
			if te.Class != ClassUnavailable {
				t.Fatalf("class = %v, want unavailable", te.Class)
			}
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("err = %v, want sentinel %v", err, tt.sentinel)
			}
			// Terminal: later calls fail fast with the same class.
			_, err2 := c.Call(testCtx(t), mcp.MethodPing, nil)
			if !errors.As(err2, &te) || te.Class != ClassUnavailable {
				t.Fatalf("conn not terminally failed: %v", err2)
			}
		})
	}
}

// A 16 MiB+ payload against the real bound, end to end through the conn.
func TestOversizedFrameRealLimit(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		_ = p.nextRequest()
		// A syntactically valid response that exceeds the frame bound.
		pad := bytes.Repeat([]byte("a"), mcp.MaxFrameSize)
		frame := append([]byte(`{"jsonrpc":"2.0","id":1,"result":"`), pad...)
		frame = append(frame, []byte("\"}\n")...)
		_, _ = p.w.Write(frame)
	}()
	_, err := c.Call(testCtx(t), mcp.MethodPing, nil)
	if !errors.Is(err, mcp.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
}

// Oversized outgoing params are rejected before hitting the wire and do
// not poison the connection.
func TestCallOversizedParamsStaysUsable(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	big := json.RawMessage(`"` + strings.Repeat("a", mcp.MaxFrameSize) + `"`)
	_, err := c.Call(testCtx(t), mcp.MethodToolsCall, big)
	if !errors.Is(err, mcp.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal (nothing was sent)", err)
	}
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`{}`)))
	}()
	if _, err := c.Call(testCtx(t), mcp.MethodPing, nil); err != nil {
		t.Fatalf("conn unusable after rejected oversized write: %v", err)
	}
}

// Server-initiated reverse RPC (roots/list) is answered inline from the
// read loop, echoing the server's id verbatim.
func TestPeerRequestInlineReply(t *testing.T) {
	tests := []struct {
		name       string
		handler    PeerHandler
		wantErrIn  int // expected error code, 0 = success expected
		wantResult string
	}{
		{
			name: "handler replies",
			handler: func(_ context.Context, req *mcp.Request) (*mcp.Response, error) {
				if req.Method != mcp.MethodRootsList {
					return nil, fmt.Errorf("unexpected method %q", req.Method)
				}
				return mcp.NewResponse(mcp.ID{}, json.RawMessage(`{"roots":[]}`)), nil
			},
			wantResult: `{"roots":[]}`,
		},
		{
			name: "handler error becomes internal error reply",
			handler: func(context.Context, *mcp.Request) (*mcp.Response, error) {
				return nil, errors.New("boom")
			},
			wantErrIn: mcp.CodeInternalError,
		},
		{
			name:      "no handler yields method not found",
			handler:   nil,
			wantErrIn: mcp.CodeMethodNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p := newPipeConn(t, mcp.MaxFrameSize)
			if tt.handler != nil {
				c.OnPeerRequest(tt.handler)
			}
			p.writeFrame(mcp.NewRequest(mcp.NewStringID("srv-1"), mcp.MethodRootsList, nil))
			resp, ok := p.next().(*mcp.Response)
			if !ok {
				t.Fatal("expected response")
			}
			if resp.ID.Key() != `"srv-1"` {
				t.Fatalf("reply id %s, want the server's id echoed", resp.ID)
			}
			if tt.wantErrIn != 0 {
				if resp.Error == nil || resp.Error.Code != tt.wantErrIn {
					t.Fatalf("error = %+v, want code %d", resp.Error, tt.wantErrIn)
				}
				return
			}
			if resp.Error != nil {
				t.Fatalf("unexpected error %v", resp.Error)
			}
			if string(resp.Result) != tt.wantResult {
				t.Fatalf("result %s", resp.Result)
			}
		})
	}
}

// Peer requests must be answerable while a client call is in flight (the
// reverse RPC arrives between our request and its response).
func TestPeerRequestInterleavedWithCall(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	c.OnPeerRequest(func(context.Context, *mcp.Request) (*mcp.Response, error) {
		return mcp.NewResponse(mcp.ID{}, json.RawMessage(`{"roots":[]}`)), nil
	})
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewRequest(mcp.NewIntID(999), mcp.MethodRootsList, nil))
		if _, ok := p.next().(*mcp.Response); !ok {
			p.t.Error("expected inline peer reply before the call response")
			return
		}
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`{}`)))
	}()
	if _, err := c.Call(testCtx(t), mcp.MethodToolsList, nil); err != nil {
		t.Fatal(err)
	}
}

// ctx cancellation forwards notifications/cancelled (best-effort) and
// returns the context error.
func TestCancelForwardsCancelledNotification(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	ctx, cancel := context.WithCancel(context.Background())
	callErr := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, mcp.MethodToolsCall, json.RawMessage(`{"name":"slow"}`))
		callErr <- err
	}()
	req := p.nextRequest() // server received the call but never answers
	cancel()
	// Read the forwarded notification first: io.Pipe writes are fully
	// synchronous, so Call only returns after the peer consumes the
	// best-effort cancelled notification.
	n, ok := p.next().(*mcp.Notification)
	if err := <-callErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("call err = %v, want context.Canceled", err)
	}
	if !ok || n.Method != mcp.NotificationCancelled {
		t.Fatalf("got %#v, want notifications/cancelled", n)
	}
	var cp mcp.CancelledParams
	if err := json.Unmarshal(n.Params, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.RequestID.Key() != req.ID.Key() {
		t.Fatalf("cancelled requestId %s, want %s", cp.RequestID, req.ID)
	}
	if cp.Reason == "" {
		t.Fatal("expected a reason")
	}
}

func TestListChangedCallback(t *testing.T) {
	tests := []struct {
		method string
		want   ChangeMask
	}{
		{mcp.NotificationToolsListChanged, ChangeTools},
		{mcp.NotificationResourcesListChanged, ChangeResources},
		{mcp.NotificationPromptsListChanged, ChangePrompts},
	}
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	got := make(chan ChangeMask, 1)
	c.OnListChanged(func(mask ChangeMask) { got <- mask })
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			p.writeFrame(mcp.NewNotification(tt.method, nil))
			select {
			case mask := <-got:
				if mask != tt.want {
					t.Fatalf("mask %v, want %v", mask, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("callback never fired")
			}
		})
	}
	// Unknown notifications are ignored without failing the conn.
	p.writeFrame(mcp.NewNotification("notifications/progress", nil))
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`{}`)))
	}()
	if _, err := c.Call(testCtx(t), mcp.MethodPing, nil); err != nil {
		t.Fatalf("conn failed after unknown notification: %v", err)
	}
}

func TestCloseFailsPendingAndIsIdempotent(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	callErr := make(chan error, 1)
	go func() {
		_, err := c.Call(testCtx(t), mcp.MethodPing, nil)
		callErr <- err
	}()
	_ = p.nextRequest()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	err := <-callErr
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable || !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call err = %v, want ClassUnavailable/ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Call(testCtx(t), mcp.MethodPing, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close = %v, want ErrClosed", err)
	}
}

func TestChangeMaskString(t *testing.T) {
	if s := (ChangeTools | ChangePrompts).String(); s != "tools|prompts" {
		t.Fatalf("got %q", s)
	}
	if s := ChangeMask(0).String(); s != "none" {
		t.Fatalf("got %q", s)
	}
}

func TestTailBuffer(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{name: "under limit", writes: []string{"ab", "cd"}, want: "abcd"},
		{name: "exactly limit", writes: []string{"abcdefgh"}, want: "abcdefgh"},
		{name: "single overflow write", writes: []string{"0123456789ab"}, want: "456789ab"},
		{name: "rolling overflow", writes: []string{"0123", "4567", "89ab"}, want: "456789ab"},
		{name: "huge write keeps tail", writes: []string{strings.Repeat("x", 100) + "TAIL-END"}, want: "TAIL-END"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTailBuffer(8)
			for _, w := range tt.writes {
				n, err := b.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write = (%d, %v)", n, err)
				}
			}
			if got := b.String(); got != tt.want {
				t.Fatalf("tail %q, want %q", got, tt.want)
			}
		})
	}
}
