package downstream

import (
	"errors"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// ErrEndpointMoved is the typed HTTP 410 Gone condition, re-exported from
// the transport facade so callers of this package never have to import it
// ("HTTP 410 Gone → typed ErrEndpointMoved, tells the user to update the URL,
// never retried").
//
// The downstream layer treats it as TERMINAL:
//
//   - never retried — 410 is not "try again", it is "this URL is dead";
//   - never respawned — re-dialing the same endpoint reproduces the 410, so
//     a half-open probe that hits it must not burn a reconnect;
//   - surfaced with the remediation hint below, because the only fix is a
//     human changing the configured URL.
var ErrEndpointMoved = transport.ErrEndpointMoved

// movedHint is the frozen remediation text appended to a 410 failure. It is
// user-facing and asserted by test (error copy is contract, docs/subsystems/downstream.md
// "determinism is contract").
const movedHint = "update the configured URL: agenthub server add <id> --url <new-url> (or edit servers.json)"

// endpointMoved reports whether err is the terminal 410 Gone condition.
func endpointMoved(err error) bool { return errors.Is(err, ErrEndpointMoved) }

// withMovedHint decorates a 410 failure with the remediation hint, leaving
// the original error unwrappable (errors.Is(err, ErrEndpointMoved) still
// holds for every caller downstream of us).
func (s *Server) withMovedHint(err error) error {
	return fmt.Errorf("downstream %q: %w — %s", s.spec.ID, err, movedHint)
}
