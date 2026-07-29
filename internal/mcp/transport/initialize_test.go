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

			res, err := Initialize(testCtx(t), c, mcp.Implementation{Name: "agenthub", Version: "test"})

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

func TestInitializeMalformedResult(t *testing.T) {
	c, p := newPipeConn(t, mcp.MaxFrameSize)
	go func() {
		req := p.nextRequest()
		p.writeFrame(mcp.NewResponse(req.ID, json.RawMessage(`"not an object"`)))
	}()
	_, err := Initialize(testCtx(t), c, mcp.Implementation{Name: "x", Version: "1"})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassFatal {
		t.Fatalf("err = %v, want ClassFatal decode failure", err)
	}
}
