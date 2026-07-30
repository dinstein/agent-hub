package transport

import (
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// negotiatedSetter is implemented by transports that can carry the MCP
// 2026-07-28 per-request _meta payload. Handshake calls it with the
// negotiated version and, for Version2026, the RequestMeta every subsequent
// outgoing message must carry; a transport that does not implement it
// cannot speak 2026-07-28 (Handshake fails closed rather than sending bare
// requests a strict server rejects with -32602).
type negotiatedSetter interface {
	setNegotiated(version string, meta *mcp.RequestMeta)
}

// injectMeta returns params with meta spliced in as the top-level _meta
// member. Only the top level is decoded: every existing value round-trips
// through json.RawMessage byte-identically, so downstream content is never
// re-encoded. Params that already carry _meta (Discover builds its own) are
// returned unchanged, and meta == nil is the pre-2026 no-op.
func injectMeta(raw json.RawMessage, meta *mcp.RequestMeta) (json.RawMessage, error) {
	if meta == nil {
		return raw, nil
	}
	top := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("params cannot carry _meta: %w", err)
		}
		if _, ok := top["_meta"]; ok {
			return raw, nil
		}
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode _meta: %w", err)
	}
	top["_meta"] = mb
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("encode params with _meta: %w", err)
	}
	return out, nil
}

// nameForHeader extracts the top-level "name" member of params for the
// Mcp-Name header (MCP 2026-07-28 requires it on the requests that carry a
// tool / resource / prompt name). Params without one — or params that are
// not an object — yield "", which suppresses the header.
func nameForHeader(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Name
}
