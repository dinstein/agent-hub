package pipeline

import (
	"context"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// shapeStage is the post-call hook: it bounds a delivered result to the
// caller's budget and retains the remainder for fetch_result.
//
// It is a Shaper rather than a gate because it runs AFTER the downstream
// answered, and it runs EXACTLY ONCE over the outcome — shaping a result
// twice would double-charge the cursor and could leave a truncation banner
// pointing at bytes nobody receives.
//
// It used to be defend_and_shape: an injection scan and a sensitive-data
// scan ran here first, and could relabel or withhold a downstream's answer.
// Both went with the rest of the runtime governance. What is left inspects
// nothing and decides nothing about a call — it only bounds how much of the
// answer travels, which is accounting, not policy.
type shapeStage struct {
	n atomic.Uint64
	// shape bounds the delivered result to the caller's budget. nil = no
	// shaping (results are delivered whole).
	shape ShapeFunc
}

func (s *shapeStage) Name() string { return StageDefendAndShape }

// Count implements Counter.
func (s *shapeStage) Count() uint64 { return s.n.Load() }

func (s *shapeStage) Shape(ctx context.Context, req *CallRequest, res *mcp.CallResult, callErr error) (*mcp.CallResult, error) {
	s.n.Add(1)
	if callErr != nil || res == nil || s.shape == nil {
		return res, callErr
	}
	if shaped := s.shape(ctx, req, res); shaped != nil {
		res = shaped
	}
	return res, nil
}
