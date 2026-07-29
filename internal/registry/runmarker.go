package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Crash marker (`internal/registry` row): the first state a
// long-running process writes is an ARMED marker; the last thing a clean
// shutdown does is RESOLVE it. The next start reads what it finds and
// reports previous_shutdown: clean | crash — which is how `agenthub doctor`
// can say "the last run did not shut down cleanly" instead of leaving the
// user to guess from a truncated log.
//
// Why the marker is rewritten rather than deleted on resolve: an ABSENT
// marker must stay distinguishable from a RESOLVED one. Deleting on
// shutdown would make "first run ever" and "clean shutdown" the same
// observation, and the first run would be reported as clean without any
// evidence.
//
// Failure direction: every ambiguity resolves toward ShutdownUnknown, never
// toward ShutdownClean. A marker we cannot read, cannot parse, or that was
// written by an unknown future version is "unknown" — the diagnostic must
// not invent a clean bill of health.

// RunMarkerName is the marker file inside the registry directory. The dot
// prefix keeps it out of the document namespace (Doc kinds are all
// "<name>.json").
const RunMarkerName = ".runstate.json"

// runMarkerVersion is the on-disk schema version. An unknown version reads
// as ShutdownUnknown rather than being reinterpreted.
const runMarkerVersion = 1

// ShutdownState is how the PREVIOUS run of a long-lived process ended.
type ShutdownState string

// Shutdown states. The strings are wire values (doctor JSON output).
const (
	// ShutdownUnknown: no marker, an unreadable marker, or a marker from a
	// schema version this build does not understand. Also the value for a
	// first-ever run.
	ShutdownUnknown ShutdownState = "unknown"
	// ShutdownClean: the previous run resolved its marker.
	ShutdownClean ShutdownState = "clean"
	// ShutdownCrash: the previous run armed a marker and never resolved it.
	ShutdownCrash ShutdownState = "crash"
)

// runMarker is the on-disk shape.
type runMarker struct {
	Version int `json:"version"`
	// Armed is true between ArmRunMarker and Resolve.
	Armed bool `json:"armed"`
	// Pid and ArmedAt identify the run that armed the marker; they are
	// diagnostics only — the crash verdict never depends on whether that pid
	// still exists (pids are reused, and on a different machine or after a
	// reboot the check is meaningless).
	Pid     int       `json:"pid"`
	ArmedAt time.Time `json:"armedAt"`
	// ResolvedAt is set by Resolve.
	ResolvedAt time.Time `json:"resolvedAt,omitempty"`
}

// RunMarker is the armed handle returned by ArmRunMarker.
type RunMarker struct {
	path string
	pid  int
}

// ArmRunMarker reads the previous run's outcome and arms a fresh marker for
// this run, atomically. It returns the PREVIOUS state — the value doctor
// reports — plus the handle whose Resolve marks this run clean.
//
// Call it once, as early as the process has a registry directory; call
// Resolve once, as the last step of a graceful shutdown. A process that is
// SIGKILLed (or panics, or loses power) simply never resolves, which is
// precisely the signal.
func ArmRunMarker(dir string) (*RunMarker, ShutdownState, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, ShutdownUnknown, err
	}
	path := filepath.Join(dir, RunMarkerName)
	prev := readRunMarker(path)
	m := &RunMarker{path: path, pid: os.Getpid()}
	data, err := json.Marshal(runMarker{
		Version: runMarkerVersion,
		Armed:   true,
		Pid:     m.pid,
		ArmedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, prev, err
	}
	if err := atomicWrite(path, data); err != nil {
		return nil, prev, fmt.Errorf("registry: arming run marker: %w", err)
	}
	return m, prev, nil
}

// Resolve marks this run as cleanly shut down. It is idempotent and safe on
// a nil marker (a process that failed to arm one still shuts down normally).
func (m *RunMarker) Resolve() error {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(runMarker{
		Version:    runMarkerVersion,
		Armed:      false,
		Pid:        m.pid,
		ResolvedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if err := atomicWrite(m.path, data); err != nil {
		return fmt.Errorf("registry: resolving run marker: %w", err)
	}
	return nil
}

// PreviousShutdown reports how the last run that armed a marker in dir
// ended, WITHOUT arming a new one. This is the read-only accessor for
// diagnostics (`agenthub doctor`) that must not perturb the state they
// report on.
func PreviousShutdown(dir string) ShutdownState {
	return readRunMarker(filepath.Join(dir, RunMarkerName))
}

// readRunMarker maps the on-disk marker onto a verdict. Everything
// ambiguous is ShutdownUnknown (see the failure direction above).
func readRunMarker(path string) ShutdownState {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ShutdownUnknown // first run in this directory
		}
		return ShutdownUnknown
	}
	var m runMarker
	if json.Unmarshal(b, &m) != nil || m.Version != runMarkerVersion {
		return ShutdownUnknown
	}
	if m.Armed {
		return ShutdownCrash
	}
	return ShutdownClean
}
