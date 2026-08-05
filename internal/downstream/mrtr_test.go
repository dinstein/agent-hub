package downstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mrtr"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// inputRequired builds one input_required tools/call answer.
func inputRequired(state string, reqs map[string]string) json.RawMessage {
	ir := mcp.InputRequiredResult{
		ResultType:    mcp.ResultTypeInputRequired,
		InputRequests: mcp.InputRequests{},
		RequestState:  &state,
	}
	for key, method := range reqs {
		ir.InputRequests[key] = mcp.InputRequest{Method: method}
	}
	raw, _ := json.Marshal(ir)
	return raw
}

// rootsPeer answers roots/list like the gateway's peer handler does.
func rootsPeer(t *testing.T, uri string) func(context.Context, *mcp.Request) (*mcp.Response, error) {
	t.Helper()
	return func(_ context.Context, req *mcp.Request) (*mcp.Response, error) {
		if req.Method != mcp.MethodRootsList {
			return mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code: mcp.CodeMethodNotFound, Message: "unhandled " + req.Method,
			}), nil
		}
		raw, _ := json.Marshal(mcp.ListRootsResult{Roots: []mcp.Root{{URI: uri}}})
		return mcp.NewResponse(req.ID, raw), nil
	}
}

// One MRTR round: input_required asking for roots/list, answered through
// the peer-handler seam, retried with the requestState echoed verbatim and
// the original name/arguments intact.
func TestCallResolvesOneInputRound(t *testing.T) {
	t.Parallel()
	s, tr := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		if method != mcp.MethodToolsCall {
			return nil, fmt.Errorf("unscripted method %q", method)
		}
		if n == 1 {
			return inputRequired("opaque-state-1", map[string]string{"r1": mcp.MethodRootsList}), nil
		}
		return json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"done"}]}`), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///workspace"))

	res, err := s.Call(testCtx(t), "echo", json.RawMessage(`{"s":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.ResultType != mcp.ResultTypeComplete {
		t.Fatalf("resultType %q", res.ResultType)
	}
	if got := tr.count(mcp.MethodToolsCall); got != 2 {
		t.Fatalf("tools/call sent %d times, want 2", got)
	}

	var retry mcp.CallToolParams
	if err := json.Unmarshal(tr.paramsOf(mcp.MethodToolsCall, 2), &retry); err != nil {
		t.Fatalf("decode retry params: %v", err)
	}
	if retry.RequestState == nil || *retry.RequestState != "opaque-state-1" {
		t.Fatalf("requestState %v, want the server's blob echoed verbatim", retry.RequestState)
	}
	if retry.Name != "echo" || string(retry.Arguments) != `{"s":"hi"}` {
		t.Fatalf("retry mutated the original call: %+v", retry)
	}
	var roots mcp.ListRootsResult
	if err := json.Unmarshal(retry.InputResponses["r1"], &roots); err != nil {
		t.Fatalf("decode inputResponses[r1]: %v", err)
	}
	if len(roots.Roots) != 1 || roots.Roots[0].URI != "file:///workspace" {
		t.Fatalf("inputResponses[r1] = %s", retry.InputResponses["r1"])
	}

	// The first call must not have carried any retry fields.
	var first mcp.CallToolParams
	if err := json.Unmarshal(tr.paramsOf(mcp.MethodToolsCall, 1), &first); err != nil {
		t.Fatal(err)
	}
	// Absent, not empty: the spec says a client MUST NOT include a
	// requestState it was never given.
	if first.RequestState != nil || first.InputResponses != nil {
		t.Fatalf("first call carried retry fields: %+v", first)
	}
}

// sampling/createMessage is refused before any handler runs.
func TestCallRejectsSamplingInput(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		return inputRequired("s", map[string]string{"llm": mcp.MethodSamplingCreate}), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///unused"))
	_, err := s.Call(testCtx(t), "echo", nil)
	// DEPRECATED-UPSTREAM(sampling, earliest-removal: 2027-07-28): asserts
	// the REFUSAL, so it outlives the feature only as long as the refusal
	// does.
	if err == nil || !strings.Contains(err.Error(), "sampling/createMessage") {
		t.Fatalf("err = %v, want the sampling refusal", err)
	}
}

// A server that never converges is cut off at the round cap.
func TestCallBoundsInputRounds(t *testing.T) {
	t.Parallel()
	s, tr := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		return inputRequired("again", map[string]string{"r": mcp.MethodRootsList}), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///w"))
	_, err := s.Call(testCtx(t), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "input_required after") {
		t.Fatalf("err = %v, want the round-cap failure", err)
	}
	// initial call + one per allowed round
	if got := tr.count(mcp.MethodToolsCall); got != 5 {
		t.Fatalf("tools/call sent %d times, want 5 (1 + 4 rounds)", got)
	}
}

// No peer handler registered: the round fails like a legacy reverse RPC
// would, with the failure naming the input.
func TestCallInputWithoutPeerHandler(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		return inputRequired("s", map[string]string{"r1": mcp.MethodRootsList}), nil
	})
	_, err := s.Call(testCtx(t), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "no peer handler") {
		t.Fatalf("err = %v, want the no-peer-handler failure", err)
	}
}

// TestCallLoadSheddingRoundRetries: an input_required carrying only
// requestState asks for nothing — it is the load-shedding shape the
// specification names, meaning "come back with this token". The client MAY
// retry immediately, and refusing made such a server unusable through this
// hub. The retry must echo the token and carry NO inputResponses, because
// nothing was requested.
func TestCallLoadSheddingRoundRetries(t *testing.T) {
	t.Parallel()
	s, tr := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		if method == mcp.MethodToolsCall && n == 1 {
			return inputRequired("shed-1", nil), nil
		}
		return json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///w"))
	res, err := s.Call(testCtx(t), "echo", json.RawMessage(`{"s":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res == nil {
		t.Fatal("no result after the shed round")
	}
	var retry mcp.CallToolParams
	if err := json.Unmarshal(tr.paramsOf(mcp.MethodToolsCall, 2), &retry); err != nil {
		t.Fatalf("decode retry params: %v", err)
	}
	if retry.RequestState == nil || *retry.RequestState != "shed-1" {
		t.Fatalf("requestState %v, want the token echoed verbatim", retry.RequestState)
	}
	if retry.InputResponses != nil {
		t.Fatalf("retry carried inputResponses for a round that requested none: %+v", retry.InputResponses)
	}
	if retry.Name != "echo" || string(retry.Arguments) != `{"s":"hi"}` {
		t.Fatalf("retry mutated the original call: %+v", retry)
	}
}

// A server that only ever sheds is not converging, and the ROUND CAP is what
// stops it — not a refusal to retry. That distinction is the whole fix: the
// loop is bounded either way, so bounding it by refusing the first shed cost
// interop and bought nothing.
func TestCallLoadSheddingForeverHitsTheRoundCap(t *testing.T) {
	t.Parallel()
	s, _ := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		return inputRequired("shed", nil), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///w"))
	_, err := s.Call(testCtx(t), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "still input_required after") {
		t.Fatalf("err = %v, want the round cap to stop it", err)
	}
}

// Neither inputRequests nor requestState is the one InputRequiredResult a
// server MUST NOT send: nothing to answer, nothing to echo, so the retry
// would be byte-identical. That still fails closed, on the first round.
func TestCallInputRequiredWithNeitherMemberFailsClosed(t *testing.T) {
	t.Parallel()
	s, tr := scriptedServer(t, downstream.Deps{}, func(method string, n int) (json.RawMessage, error) {
		return json.RawMessage(`{"resultType":"input_required"}`), nil
	})
	s.OnPeerRequest(rootsPeer(t, "file:///w"))
	_, err := s.Call(testCtx(t), "echo", nil)
	if !errors.Is(err, mrtr.ErrNoInputRequests) {
		t.Fatalf("err = %v, want ErrNoInputRequests", err)
	}
	if got := tr.count(mcp.MethodToolsCall); got != 1 {
		t.Fatalf("made %d tools/call attempts, want 1 — there was nothing to retry with", got)
	}
}
