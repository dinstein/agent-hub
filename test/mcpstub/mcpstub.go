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
// keys, or whose MCP-Protocol-Version / Mcp-Method / Mcp-Name header disagrees
// with the body, is rejected with the spec's error code. A client test that passes against this stub has
// therefore proven wire-level conformance, not just happy-path agreement.
type Server struct {
	hs *httptest.Server

	mu    sync.Mutex
	calls map[string]int
	// issued maps every requestState this stub has handed out to the round
	// it belongs to. A retry must echo the blob verbatim, use a DIFFERENT
	// JSON-RPC id, and repeat the original arguments; anything else is
	// rejected, which is what makes the integration test a proof that the
	// client treats the blob as opaque and re-issues the original request
	// rather than a new one.
	issued   map[string]*round
	stateSeq int
}

// New starts the stub. Callers own Close.
func New() *Server {
	s := &Server{calls: map[string]int{}, issued: map[string]*round{}}
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
	Meta *rawMeta `json:"_meta"`
}

// rawMeta keeps each key as raw JSON so ABSENT is distinguishable from an
// empty object. mcp.RequestMeta cannot do that: ClientCapabilities is a
// non-pointer struct, so a client that omitted the key entirely decodes
// identically to one that sent {} — and this stub used to name that key in a
// rejection message for a check it could not perform.
type rawMeta struct {
	ProtocolVersion    string          `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
}

// round is one outstanding MRTR exchange: the id the input_required went out
// on, and the arguments of the request that produced it.
type round struct {
	outstanding bool
	requestID   string
	arguments   string
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
	case mp.Meta.ClientCapabilities == nil:
		// Required on EVERY request, and an empty object is the correct way
		// to say "no optional capabilities" — omitting the key is not.
		s.reject(w, req, http.StatusBadRequest, mcp.CodeInvalidParams,
			"_meta carries no io.modelcontextprotocol/clientCapabilities; it is required on every request")
		return
	case mp.Meta.ProtocolVersion != mcp.Version2026:
		// -32022 carries its supported/requested payload: a client is told
		// to retry with a version from the list, so a stub that omitted it
		// would let a client that cannot read one still pass.
		writeMessage(w, http.StatusBadRequest, mcp.NewErrorResponse(req.ID,
			mcp.NewUnsupportedVersionError(mp.Meta.ProtocolVersion, []string{mcp.Version2026},
				fmt.Sprintf("protocol version %q, this server speaks %q only",
					mp.Meta.ProtocolVersion, mcp.Version2026))))
		return
	}
	// Required headers: MCP-Protocol-Version, which MUST equal what the
	// body's _meta declared; Mcp-Method always; Mcp-Name when params carry
	// one. The version check is the one a client can only fail before it has
	// negotiated anything, so a stub that skips it certifies nothing.
	if got := r.Header.Get("MCP-Protocol-Version"); got != mp.Meta.ProtocolVersion {
		s.reject(w, req, http.StatusBadRequest, mcp.CodeHeaderMismatch,
			fmt.Sprintf("MCP-Protocol-Version header %q, body _meta declares %q",
				got, mp.Meta.ProtocolVersion))
		return
	}
	if got := r.Header.Get("Mcp-Method"); got != req.Method {
		s.reject(w, req, http.StatusBadRequest, mcp.CodeHeaderMismatch,
			fmt.Sprintf("Mcp-Method header %q, body method %q", got, req.Method))
		return
	}

	switch req.Method {
	case mcp.MethodDiscover:
		dttl := int64(3_600_000)
		s.ok(w, req, mcp.DiscoverResult{
			ResultType:        mcp.ResultTypeComplete,
			SupportedVersions: []string{mcp.Version2026},
			Capabilities:      json.RawMessage(`{"tools":{}}`),
			Meta: &mcp.ResultMeta{
				ServerInfo: &mcp.Implementation{Name: "mcpstub", Version: "1"},
			},
			CacheableResult: mcp.CacheableResult{TtlMs: &dttl, CacheScope: "public"},
		})
	case mcp.MethodToolsList:
		ttl := int64(60_000)
		s.ok(w, req, mcp.ListToolsResult{
			ResultType: mcp.ResultTypeComplete,
			Tools: []mcp.ToolDef{{
				Name:        "echo",
				Description: "echoes its arguments back",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, {
				Name:        "confirm",
				Description: "requires one MRTR round (roots/list) before answering",
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
		// Decode before comparing: a name outside the header-safe set
		// travels under the base64 sentinel, and a stub that compared the
		// raw text would fail the clients that got it right.
		got, ok := mcp.DecodeHeaderValue(r.Header.Get("Mcp-Name"))
		if !ok || got != p.Name {
			s.reject(w, req, http.StatusBadRequest, mcp.CodeHeaderMismatch,
				fmt.Sprintf("Mcp-Name header %q, params name %q", r.Header.Get("Mcp-Name"), p.Name))
			return
		}
		switch p.Name {
		case "echo":
			content, _ := json.Marshal([]map[string]string{{"type": "text", "text": string(p.Arguments)}})
			s.ok(w, req, mcp.CallResult{ResultType: mcp.ResultTypeComplete, Content: content})
		case "confirm":
			s.answerConfirm(w, req, p)
		default:
			s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name))
		}
	default:
		// initialize, ping and everything else pre-2026: removed methods on
		// a stateless server are simply unknown.
		s.reject(w, req, http.StatusOK, mcp.CodeMethodNotFound,
			fmt.Sprintf("mcpstub (2026-07-28) does not implement %q", req.Method))
	}
}

// answerConfirm drives one MRTR round: the first call gets input_required
// asking for roots/list; the retry must echo the issued requestState
// verbatim and answer every input key, or it is rejected.
func (s *Server) answerConfirm(w http.ResponseWriter, req *mcp.Request, p mcp.CallToolParams) {
	if p.RequestState == nil {
		s.mu.Lock()
		s.stateSeq++
		state := fmt.Sprintf("mcpstub-opaque-%d", s.stateSeq)
		s.issued[state] = &round{
			outstanding: true,
			requestID:   req.ID.Key(),
			arguments:   string(p.Arguments),
		}
		s.mu.Unlock()
		result, _ := json.Marshal(mcp.InputRequiredResult{
			ResultType: mcp.ResultTypeInputRequired,
			InputRequests: mcp.InputRequests{
				"roots": {Method: mcp.MethodRootsList},
			},
			RequestState: &state,
		})
		writeMessage(w, http.StatusOK, mcp.NewResponse(req.ID, result))
		return
	}
	s.mu.Lock()
	rd := s.issued[*p.RequestState]
	if rd != nil && rd.outstanding {
		rd.outstanding = false
	} else {
		rd = nil
	}
	s.mu.Unlock()
	if rd == nil {
		s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams,
			fmt.Sprintf("requestState %q was never issued (or already redeemed): the client must echo it verbatim", *p.RequestState))
		return
	}
	// "The JSON-RPC id MUST be different between the initial request and the
	// retry, as they are independent requests." This stub is the only place
	// in the tree that sees both ids on the wire, so it is the only place
	// that rule can be observed rather than assumed.
	if req.ID.Key() == rd.requestID {
		s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams,
			fmt.Sprintf("retry reused JSON-RPC id %s; the retry is an independent request and must use a new one",
				rd.requestID))
		return
	}
	// The retry re-issues the ORIGINAL request. Arguments that changed
	// between the rounds mean the client rebuilt the call instead of
	// resuming it, which the server's own state was computed against.
	if got := string(p.Arguments); got != rd.arguments {
		s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams,
			fmt.Sprintf("retry arguments %s differ from the original %s; the retry must re-issue the original request",
				got, rd.arguments))
		return
	}
	rootsRaw, ok := p.InputResponses["roots"]
	if !ok {
		s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams,
			`retry is missing inputResponses["roots"]`)
		return
	}
	var roots mcp.ListRootsResult
	if err := json.Unmarshal(rootsRaw, &roots); err != nil {
		s.reject(w, req, http.StatusOK, mcp.CodeInvalidParams,
			"inputResponses[roots] does not decode as a roots/list result: "+err.Error())
		return
	}
	content, _ := json.Marshal([]map[string]string{{
		"type": "text",
		"text": fmt.Sprintf("confirmed with %d root(s)", len(roots.Roots)),
	}})
	s.ok(w, req, mcp.CallResult{ResultType: mcp.ResultTypeComplete, Content: content})
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
