package transport

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

func TestInitializeNegotiation(t *testing.T) {
	tests := []struct {
		name          string
		serverVersion string
		wantVersion   string
		wantErr       bool
	}{
		{name: "exact 2025-11-25", serverVersion: "2025-11-25", wantVersion: "2025-11-25"},
		{name: "downgrade 2025-06-18", serverVersion: "2025-06-18", wantVersion: "2025-06-18"},
		{name: "downgrade 2025-03-26", serverVersion: "2025-03-26", wantVersion: "2025-03-26"},
		{name: "unsupported old", serverVersion: "2024-11-05", wantErr: true},
		{name: "unsupported garbage", serverVersion: "v99", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p := newPipeConn(t, mcp.MaxFrameSize)
			serverSaw := make(chan mcp.InitializeParams, 1)
			gotInitialized := make(chan bool, 1)
			go func() {
				req := p.nextRequest()
				if req.Method != mcp.MethodInitialize {
					p.t.Errorf("first request %q, want initialize", req.Method)
				}
				var ip mcp.InitializeParams
				if err := json.Unmarshal(req.Params, &ip); err != nil {
					p.t.Errorf("decode initialize params: %v", err)
				}
				serverSaw <- ip
				result, _ := json.Marshal(mcp.InitializeResult{
					ProtocolVersion: tt.serverVersion,
					Capabilities:    json.RawMessage(`{"tools":{"listChanged":true}}`),
					ServerInfo:      mcp.Implementation{Name: "fake", Version: "0"},
				})
				p.writeFrame(mcp.NewResponse(req.ID, result))
				if tt.wantErr {
					return // client fails before sending initialized
				}
				n, ok := p.next().(*mcp.Notification)
				gotInitialized <- ok && n.Method == mcp.NotificationInitialized
			}()

			res, err := initializeLegacy(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})

			ip := <-serverSaw
			if ip.ProtocolVersion != mcp.ProtocolVersion {
				t.Fatalf("client declared %q, want %q", ip.ProtocolVersion, mcp.ProtocolVersion)
			}
			if ip.ClientInfo.Name != "agenthub" {
				t.Fatalf("clientInfo %+v", ip.ClientInfo)
			}

			if tt.wantErr {
				if !errors.Is(err, mcp.ErrUnsupportedVersion) {
					t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
				}
				var te *Error
				if !errors.As(err, &te) || te.Class != ClassFatal {
					t.Fatalf("err = %v, want ClassFatal (handshake failure must not trip the breaker)", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.ProtocolVersion != tt.wantVersion {
				t.Fatalf("negotiated %q, want %q", res.ProtocolVersion, tt.wantVersion)
			}
			if res.ServerInfo.Name != "fake" {
				t.Fatalf("serverInfo %+v", res.ServerInfo)
			}
			if !<-gotInitialized {
				t.Fatal("server never received notifications/initialized")
			}
		})
	}
}

func TestHandshakeDiscover2026(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	paramsSeen := make(chan mcp.DiscoverParams, 1)
	after := make(chan *mcp.Request, 1)
	go func() {
		req := p.nextRequest()
		if req.Method != mcp.MethodDiscover {
			p.t.Errorf("first request %q, want server/discover", req.Method)
		}
		var dp mcp.DiscoverParams
		if err := json.Unmarshal(req.Params, &dp); err != nil {
			p.t.Errorf("decode discover params: %v", err)
		}
		paramsSeen <- dp
		result, _ := json.Marshal(mcp.DiscoverResult{
			ResultType:        mcp.ResultTypeComplete,
			SupportedVersions: []string{"2026-07-28", "2025-11-25"},
			Capabilities:      json.RawMessage(`{"tools":{"listChanged":true}}`),
			Instructions:      "the stub speaks 2026",
			Meta:              &mcp.ResultMeta{ServerInfo: &mcp.Implementation{Name: "stub2026", Version: "1"}},
		})
		p.writeFrame(mcp.NewResponse(req.ID, result))
		// The stateless path must not send notifications/initialized: the
		// next frame after the handshake has to be the follow-up request.
		next := p.nextRequest()
		after <- next
		p.writeFrame(mcp.NewResponse(next.ID, json.RawMessage(`{"tools":[]}`)))
	}()

	res, err := Handshake(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dp := <-paramsSeen
	if dp.Meta == nil {
		t.Fatal("discover request carried no _meta")
	}
	if dp.Meta.ProtocolVersion != mcp.Version2026 {
		t.Fatalf("_meta protocolVersion %q, want %q", dp.Meta.ProtocolVersion, mcp.Version2026)
	}
	if dp.Meta.ClientInfo == nil || dp.Meta.ClientInfo.Name != "agenthub" {
		t.Fatalf("_meta clientInfo %+v", dp.Meta.ClientInfo)
	}
	if res.Version != mcp.Version2026 {
		t.Fatalf("negotiated %q, want %q", res.Version, mcp.Version2026)
	}
	if res.ServerInfo.Name != "stub2026" {
		t.Fatalf("serverInfo %+v", res.ServerInfo)
	}
	if _, err := c.Call(testCtx(t), mcp.MethodToolsList, nil); err != nil {
		t.Fatal(err)
	}
	next := <-after
	if next.Method != mcp.MethodToolsList {
		t.Fatalf("frame after handshake was %q, want %q (stateless path must not send notifications/initialized)", next.Method, mcp.MethodToolsList)
	}
	// Post-handshake requests carry the negotiated _meta, even with nil
	// caller params.
	var lp struct {
		Meta *mcp.RequestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(next.Params, &lp); err != nil {
		t.Fatalf("decode tools/list params %s: %v", next.Params, err)
	}
	if lp.Meta == nil || lp.Meta.ProtocolVersion != mcp.Version2026 {
		t.Fatalf("tools/list _meta = %+v, want protocolVersion %q", lp.Meta, mcp.Version2026)
	}
}

func TestHandshakeFallbackToInitialize(t *testing.T) {
	tests := []struct {
		name  string
		reply func(p *fakePeer, req *mcp.Request)
	}{
		{name: "method not found", reply: func(p *fakePeer, req *mcp.Request) {
			p.writeFrame(mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code: mcp.CodeMethodNotFound, Message: "method not found",
			}))
		}},
		// Some pre-2026 servers reject any request before initialize with a
		// generic error instead of method-not-found; any JSON-RPC error
		// reply proves the server is alive and old.
		{name: "pre-initialize rejection", reply: func(p *fakePeer, req *mcp.Request) {
			p.writeFrame(mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code: mcp.CodeInvalidRequest, Message: "received request before initialization was complete",
			}))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p := newPipeConn(t, mcp.MaxFrameSize)
			gotInitialized := make(chan bool, 1)
			go func() {
				tt.reply(p, p.nextRequest())
				req := p.nextRequest()
				if req.Method != mcp.MethodInitialize {
					p.t.Errorf("fallback request %q, want initialize", req.Method)
				}
				result, _ := json.Marshal(mcp.InitializeResult{
					ProtocolVersion: mcp.Version2025,
					Capabilities:    json.RawMessage(`{}`),
					ServerInfo:      mcp.Implementation{Name: "legacy", Version: "0"},
					Instructions:    "be nice",
				})
				p.writeFrame(mcp.NewResponse(req.ID, result))
				n, ok := p.next().(*mcp.Notification)
				gotInitialized <- ok && n.Method == mcp.NotificationInitialized
			}()

			res, err := Handshake(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if res.Version != mcp.Version2025 {
				t.Fatalf("negotiated %q, want %q", res.Version, mcp.Version2025)
			}
			if res.ServerInfo.Name != "legacy" || res.Instructions != "be nice" {
				t.Fatalf("result %+v", res)
			}
			if !<-gotInitialized {
				t.Fatal("server never received notifications/initialized")
			}
		})
	}
}

func TestHandshakeDiscoverNegotiatesLegacy(t *testing.T) {
	// A server may implement server/discover yet only offer pre-2026
	// versions; those still require the stateful initialize handshake.
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	gotInitialized := make(chan bool, 1)
	go func() {
		req := p.nextRequest()
		result, _ := json.Marshal(mcp.DiscoverResult{
			SupportedVersions: []string{"2025-11-25", "2025-06-18"},
			Meta:              &mcp.ResultMeta{ServerInfo: &mcp.Implementation{Name: "mixed", Version: "1"}},
		})
		p.writeFrame(mcp.NewResponse(req.ID, result))
		req = p.nextRequest()
		if req.Method != mcp.MethodInitialize {
			p.t.Errorf("second request %q, want initialize", req.Method)
		}
		initResult, _ := json.Marshal(mcp.InitializeResult{
			ProtocolVersion: mcp.Version2025,
			Capabilities:    json.RawMessage(`{}`),
			ServerInfo:      mcp.Implementation{Name: "mixed", Version: "1"},
		})
		p.writeFrame(mcp.NewResponse(req.ID, initResult))
		n, ok := p.next().(*mcp.Notification)
		gotInitialized <- ok && n.Method == mcp.NotificationInitialized
	}()

	res, err := Handshake(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != mcp.Version2025 {
		t.Fatalf("negotiated %q, want %q", res.Version, mcp.Version2025)
	}
	if !<-gotInitialized {
		t.Fatal("server never received notifications/initialized")
	}
}

func TestHandshakeDiscoverNoMutualVersion(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		req := p.nextRequest()
		result, _ := json.Marshal(mcp.DiscoverResult{
			SupportedVersions: []string{"2099-01-01"},
			Meta:              &mcp.ResultMeta{ServerInfo: &mcp.Implementation{Name: "future", Version: "9"}},
		})
		p.writeFrame(mcp.NewResponse(req.ID, result))
	}()
	_, err := Handshake(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})
	if !errors.Is(err, mcp.ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal (handshake failure must not trip the breaker)", err)
	}
}

func TestHandshakeConnectionFailurePropagates(t *testing.T) {
	// A dead connection is not an old server: no initialize fallback, the
	// error must reach the circuit breaker unchanged.
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		p.nextRequest()
		_ = p.w.Close() // connection drops before any reply
	}()
	_, err := Handshake(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
}

func TestInitializeMalformedResult(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`"not an object"`)))
	}()
	_, err := initializeLegacy(testCtx(t), c, mcp.Implementation{Name: "x", Version: "1"})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal decode failure", err)
	}
}
