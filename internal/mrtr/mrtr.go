package mrtr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Handler answers one input request by method. internal/downstream fills
// this seam with the same peer-handler adapter that serves legacy
// server-initiated reverse RPCs, so both protocol generations answer
// roots/list (and reject everything unimplemented) identically.
type Handler func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)

// ErrSamplingUnsupported rejects sampling/createMessage input requests.
// AgentHub does not proxy LLM calls (docs/mcp-2026-07-28.md §6.2), and the
// clientCapabilities it declares never include sampling — a server asking
// anyway gets this, which callers surface as
// CodeMissingRequiredClientCapability (-32021) semantics.
var ErrSamplingUnsupported = errors.New(
	"sampling/createMessage is not supported: AgentHub does not proxy LLM calls")

// ErrNoInputRequests rejects an InputRequiredResult that carries no input
// requests: answering nothing and retrying the identical request could only
// loop, so the round fails instead (fail closed).
var ErrNoInputRequests = errors.New("input_required result carried no inputRequests")

// Resolve answers every input request of one MRTR round and returns the
// inputResponses map for the retry. Requests are answered sequentially in
// sorted key order — deterministic ordering keeps any human-facing side
// effects (HITL prompts) stable across runs. The first failure aborts the
// round: a partially answered retry would be indistinguishable from a
// client that ignored a required input, so no partial map is ever returned
// (fail closed).
//
// requestState is NOT handled here on purpose: Resolve never sees it, which
// makes "the coordinator cannot inspect or modify it" a property of the
// package boundary rather than of reviewer vigilance.
func Resolve(ctx context.Context, reqs mcp.InputRequests, h Handler) (mcp.InputResponses, error) {
	if len(reqs) == 0 {
		return nil, ErrNoInputRequests
	}
	if h == nil {
		return nil, errors.New("mrtr: nil Handler")
	}
	out := make(mcp.InputResponses, len(reqs))
	for _, key := range slices.Sorted(maps.Keys(reqs)) {
		req := reqs[key]
		if req.Method == mcp.MethodSamplingCreate {
			return nil, fmt.Errorf("input %q: %w", key, ErrSamplingUnsupported)
		}
		raw, err := h(ctx, req.Method, req.Params)
		if err != nil {
			return nil, fmt.Errorf("input %q (%s): %w", key, req.Method, err)
		}
		out[key] = raw
	}
	return out, nil
}
