package cli

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/skills"
)

// TestEveryStoreLockTimeoutIsExitLocked pins the one answer an operator gets
// for lock contention, whichever store was busy.
//
// Seven packages take a cross-process flock; four of them can time out on it
// and report that to a user: registry, integrity, skills and httpbridge's
// token store. (The other three cannot — audit and ratelimit block on
// LOCK_EX, oauthflow tries once and gives up — which is why declaring
// ErrLockTimeout, not owning a flock, is what puts a store in this table.)
// Each of the four has its own copy of the lock ladder, because none of them
// may import another's document model, so nothing but a test makes them agree
// on what a timeout LOOKS like from outside.
//
// They did not agree. httpbridge returned a plain fmt.Errorf, so `agenthub
// token create` under contention fell through its classifier's default branch
// and exited 1 with a raw message, while the identical situation on any other
// store exited 7 with a retry hint. Nothing documented that difference; it was
// the one store whose ladder had never been given a typed error.
//
// The reason to pin it here rather than trust four matching implementations:
// a divergence is cheap to fix at the moment it appears and expensive later.
// This table cannot notice a FIFTH store on its own — it is hand-written, and
// a new package declaring its own ErrLockTimeout changes nothing here.
// test/buildrules' TestEveryLockTimeoutStoreIsInTheParityTable is what
// notices, by failing until the new store is named below.
func TestEveryStoreLockTimeoutIsExitLocked(t *testing.T) {
	const timeout = 10 * time.Second

	cases := []struct {
		store    string
		err      error
		classify func(error) error
	}{
		{
			store:    "registry",
			err:      &registry.LockTimeoutError{Path: "/x/registry.lock", Timeout: timeout},
			classify: func(err error) error { return err },
		},
		{
			store:    "integrity",
			err:      &integrity.LockTimeoutError{Path: "/x/tool-approvals.json.lock", Timeout: timeout},
			classify: func(err error) error { return classifyIntegrityError(err, "github", "read_file") },
		},
		{
			store:    "skills",
			err:      &skills.LockTimeoutError{Path: "/x/installs.json.lock", Timeout: timeout},
			classify: func(err error) error { return classifySkillsError(err, "some-skill") },
		},
		{
			store:    "httpbridge",
			err:      &httpbridge.LockTimeoutError{Path: "/x/tokens.json.lock", Timeout: timeout},
			classify: classifyTokenError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.store, func(t *testing.T) {
			if got := ExitCodeFor(tc.classify(tc.err)); got != ExitLocked {
				t.Errorf("a %s lock timeout exits %d, want %d (ExitLocked).\n"+
					"Every store's contention has to look the same to an operator: "+
					"give the ladder a typed *LockTimeoutError and map it in the classifier.",
					tc.store, got, ExitLocked)
			}
		})
	}
}

// TestLockTimeoutErrorsCarryTheirSentinel checks the errors.Is contract each
// classifier depends on. A LockTimeoutError whose Is method stopped matching
// would make every case above fall back to a generic exit code, and it would do
// so silently, because the type still exists and still formats correctly.
func TestLockTimeoutErrorsCarryTheirSentinel(t *testing.T) {
	const timeout = time.Second
	cases := []struct {
		store    string
		err      error
		sentinel error
	}{
		{"registry", &registry.LockTimeoutError{Path: "/x", Timeout: timeout}, registry.ErrLockTimeout},
		{"integrity", &integrity.LockTimeoutError{Path: "/x", Timeout: timeout}, integrity.ErrLockTimeout},
		{"skills", &skills.LockTimeoutError{Path: "/x", Timeout: timeout}, skills.ErrLockTimeout},
		{"httpbridge", &httpbridge.LockTimeoutError{Path: "/x", Timeout: timeout}, httpbridge.ErrLockTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.store, func(t *testing.T) {
			if !errorsIs(tc.err, tc.sentinel) {
				t.Errorf("%s: errors.Is(*LockTimeoutError, ErrLockTimeout) is false; "+
					"the Is method is what every classifier matches on", tc.store)
			}
			// The message has to name the elapsed timeout: an operator reading
			// "timed out" without a duration cannot tell contention from a hang.
			if msg := tc.err.Error(); !containsDuration(msg, timeout) {
				t.Errorf("%s: message %q does not mention the %s timeout", tc.store, msg, timeout)
			}
		})
	}
}

// errorsIs and containsDuration are tiny wrappers kept local so the table above
// reads as a table.
func errorsIs(err, target error) bool { return stderrors.Is(err, target) }

func containsDuration(msg string, d time.Duration) bool {
	return strings.Contains(msg, d.String())
}
