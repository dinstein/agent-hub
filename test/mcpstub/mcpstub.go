package mcpstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Server is an in-process MCP 2026-07-28 streamable-HTTP server. It is
// deliberately strict: every request missing the required per-request _meta
// keys, or whose Mcp-Method header disagrees with the body, is rejected with
// the spec's error code. A client test that passes against this stub has
// therefore proven wire-level conformance, not just happy-path agreement.
type Server struct {
	hs *httptest.Server

	mu    sync.Mutex
	calls map[string]int
}

// New starts the stub. Callers own Close.
func New() *Server {
	s := &Server{calls: map[string]int{}}
	s.hs = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the MCP endpoint.
func (s *Server) URL() string { return s.hs.URL }

// Close shuts the stub down.
func (s *Server) Close() { s.hs.Close() }

// Calls reports how many requests for method were received (notifications
// included), so tests can assert e.g. that initialize was never sent.
func (s *Server) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *Server) record(method string) {
	s.mu.Lock()
	s.calls[method]++
	s.mu.Unlock()
}

// metaParams is the _meta envelope every 2026-07-28 request must carry.
type metaParams struct {
	Meta *mcp.RequestMeta `json:"_meta"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg, perr := mcp.ParseMessage(body)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusBadRequest)
		return
	}
	switch m := msg.(type) {
	case *mcp.Notification:
		s.record(m.Method)
		w.WriteHeader(http.StatusAccepted)
	case *mcp.Request:
		s.record(m.Method)
		s.answer(w, r, m)
	default:
		http.Error(w, fmt.Sprintf("unexpected message %T", msg), http.StatusBadRequest)
	}
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request, req *mcp.Request) {
	// Stateless protocol: the three required _meta keys, on every request.
	var mp metaParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &mp); err != nil {
			s.reject(w, req, http.StatusBadRequest, mcp.CodeInvalidParams, "params: "+err.Error())
			return
		}
	}
	switch {
	case mp.Meta == nil:
		s.reject(w, req, http.StatusBadRequest, mcp.CodeInvalidParams,
			"missing required _meta (io.modelcontextprotocol/protocolVersion, /clientCapabilities)")
		return
	case mp.Meta.ProtocolVersion != mcp.Version2026:
		s.reject(w, req, http.StatusBadRequest, mcp.CodeUnsupportedProtocolVersion,
			fmt.Sprintf("protocol version %q, this server speaks %q only", mp.Meta.ProtocolVersion, mcp.Version2026))
		return
	}
	// Required headers: Mcp-Method always, Mcp-Name when params carry one.
	if got := r.Header.Get("Mcp-Method"); got != req.Method {
		s.reject(w, req, http.StatusBadRequest, mcp.CodeHeaderMismatch,
			fmt.Sprintf("Mcp-Method header %q, body method %q", got, req.Method))
		return
	}

	switch req.Method {
	case mcp.MethodDiscover:
		s.ok(w, req, mcp.DiscoverResult{
			ResultType:       mcp.ResultTypeComplete,
			ProtocolVersions: []string{mcp.Version2026},
			Capabilities:     json.RawMessage(`{"tools":{}}`),
			ServerInfo:       mcp.Implementation{Name: "mcpstub", Version: "1"},
		})
	case mcp.MethodToolsList:
		ttl := int64(60_000)
		s.ok(w, req, mcp.ListToolsResult{
			ResultType: mcp.ResultTypeComplete,
			Tools: []mcp.ToolDef{{
				Name:        "echo",
				Description: "echoes its arguments back",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			CacheableResult: mcp.CacheableResult{TtlMs: &ttl, CacheScope: "private"},
		})
	case mcp.MethodToolsCall:
		var p mcp.CallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.reject(w, req, http.StatusBadRequest, mcp.CodeInvalidParams, "tools/call params: "+err.Error())
			return
		}
		if got := r.Header.Get("Mcp-Name"); got != p.Name {
			s.reject(w, req, http.StatusBadRequest, mcp.CodeHeaderMismatch,
				fmt.Sprintf("Mcp-Name header %q, params name %q", got, p.Name))
			return
		}
		if p.Name != "echo" {
			s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name))
			return
		}
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": string(p.Arguments)}})
		s.ok(w, req, mcp.CallResult{ResultType: mcp.ResultTypeComplete, Content: content})
	default:
		// initialize, ping and everything else pre-2026: removed methods on
		// a stateless server are simply unknown.
		s.reject(w, req, http.StatusOK, mcp.CodeMethodNotFound,
			fmt.Sprintf("mcpstub (2026-07-28) does not implement %q", req.Method))
	}
}

func (s *Server) ok(w http.ResponseWriter, req *mcp.Request, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeMessage(w, http.StatusOK, mcp.NewResponse(req.ID, raw))
}

func (s *Server) reject(w http.ResponseWriter, req *mcp.Request, status, code int, msg string) {
	writeMessage(w, status, mcp.NewErrorResponse(req.ID, &mcp.Error{Code: code, Message: msg}))
}

func writeMessage(w http.ResponseWriter, status int, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(mcp.MaxFrameSize)+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > mcp.MaxFrameSize {
		return nil, fmt.Errorf("body exceeds frame bound")
	}
	return data, nil
}
