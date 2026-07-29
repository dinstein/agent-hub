package downstream

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// codeRateLimited is the JSON-RPC error code downstream treats as a
// rate-limit signal. M0 discretionary decision: the stdio transport never
// produces transport.ClassRetry on its own (that class arrives natively
// with the HTTP transports in M1), but some stdio servers surface HTTP-ish
// 429s as JSON-RPC errors; mapping code 429 to ClassRetry gives them — and
// the tests — the frozen retry semantics today.
const codeRateLimited = 429

// classify maps an error from a transport round trip to the retry/breaker
// class and an optional RetryAfter hint. Errors that are not
// *transport.Error (context errors, decode errors) classify as ClassFatal:
// never retried, never counted by the breaker.
func classify(err error) (transport.Class, time.Duration) {
	var te *transport.Error
	if !errors.As(err, &te) {
		return transport.ClassFatal, 0
	}
	if te.Class == transport.ClassFatal {
		var me *mcp.Error
		if errors.As(te.Err, &me) && me.Code == codeRateLimited {
			return transport.ClassRetry, retryAfterHint(me.Data)
		}
	}
	return te.Class, te.RetryAfter
}

// deadConnection reports whether err is a call the transport rejected
// BEFORE putting it on the wire because the connection had already failed
// (transport.ErrDeadConnection). Such a request provably never reached the
// server, so rebuilding the connection and replaying it cannot
// double-execute a non-idempotent tools/call.
//
// FAIL-CLOSED: anything not explicitly marked pre-send answers false, so an
// unrecognized failure is treated as possibly-executed and is never
// replayed. A post-send stream death (request written, reply lost) must
// keep answering false — that is the whole point of the marker.
func deadConnection(err error) bool {
	return errors.Is(err, transport.ErrDeadConnection)
}

// retryAfterHint extracts a retry-after hint from a 429 error's data
// object. Both {"retryAfterMs": N} (milliseconds) and {"retryAfter": N}
// (seconds) are accepted; anything else yields 0 (use default backoff).
func retryAfterHint(data json.RawMessage) time.Duration {
	if len(data) == 0 {
		return 0
	}
	var v struct {
		RetryAfterMs float64 `json:"retryAfterMs"`
		RetryAfter   float64 `json:"retryAfter"`
	}
	if json.Unmarshal(data, &v) != nil {
		return 0
	}
	switch {
	case v.RetryAfterMs > 0:
		return time.Duration(v.RetryAfterMs * float64(time.Millisecond))
	case v.RetryAfter > 0:
		return time.Duration(v.RetryAfter * float64(time.Second))
	default:
		return 0
	}
}

// backoff computes the wait before retry number attempt (1-based count of
// attempts already made). A server-provided RetryAfter is honored and
// jittered upward (never shortened); otherwise exponential backoff with
// 50–100% jitter, capped at MaxDelay.
func backoff(cfg RetryConfig, attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter + rand.N(retryAfter/4+time.Millisecond)
	}
	d := cfg.BaseDelay << (attempt - 1)
	if d > cfg.MaxDelay || d <= 0 {
		d = cfg.MaxDelay
	}
	return d/2 + rand.N(d/2+1)
}
