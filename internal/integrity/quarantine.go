package integrity

import (
	"context"
	"fmt"
	"time"
)

// QuarantineEntry is one quarantined tool.
//
// Entries are keyed (in the store) by the CLIENT-VISIBLE exposed name,
// computed AFTER per-scope overrides are applied — override precedes policy
// (#423). Rationale: quarantine must track what the agent can actually see
// and call; keying raw names once let a rename move a tool completely out
// from under integrity (the #423 incident). Callers are responsible for
// passing the post-override exposed name; Server/Tool below keep the raw
// route so release can re-baseline the right pin.
type QuarantineEntry struct {
	Server string `json:"server"` // server ID (raw route)
	Tool   string `json:"tool"`   // raw downstream tool name (raw route)

	Reason      string `json:"reason,omitempty"` // human-readable cause (drift severity, etc.)
	PinnedHash  string `json:"pinnedHash,omitempty"`
	CurrentHash string `json:"currentHash,omitempty"`

	At time.Time `json:"at"` // when quarantined (UTC)
}

// quarantineFile is the on-disk envelope of state/quarantine.json:
// exposed name -> entry.
type quarantineFile struct {
	Version int                        `json:"version"`
	Entries map[string]QuarantineEntry `json:"entries"`
}

// QuarantineStore persists the quarantine set in <state>/quarantine.json,
// guarded by a sibling cross-process lock.
//
// Fail direction of EVERY method: a corrupt store returns ErrStoreCorrupt
// and the caller MUST fail closed — treat affected tools as blocked/hidden,
// never as "nothing is quarantined". The file is never renamed aside
// (a ".corrupt" rename would make the next read look like a legitimate
// empty set — toolport's historical bug).
type QuarantineStore struct {
	f *lockedFile
}

// OpenQuarantineStore opens the quarantine store under stateDir — normally
// platform.StateDir().
func OpenQuarantineStore(stateDir string, opts Options) (*QuarantineStore, error) {
	f, err := newLockedFile(stateDir, quarantineFileName, opts)
	if err != nil {
		return nil, err
	}
	return &QuarantineStore{f: f}, nil
}

// Add quarantines exposed. An existing entry is overwritten (a repeat drift
// refreshes hashes/reason). At is stamped by the store clock.
func (s *QuarantineStore) Add(ctx context.Context, exposed string, e QuarantineEntry) error {
	if exposed == "" {
		return fmt.Errorf("integrity: quarantine: exposed name must not be empty")
	}
	return s.f.withLock(ctx, func() error {
		file, _, err := loadStore[quarantineFile](s.f.path, func(v *quarantineFile) int { return v.Version })
		if err != nil {
			return err
		}
		if file.Entries == nil {
			file.Entries = map[string]QuarantineEntry{}
		}
		e.At = s.f.now().UTC()
		file.Entries[exposed] = e
		file.Version = storeVersion
		return s.f.save(file)
	})
}

// Release removes exposed from the quarantine set (the human re-approve
// step; the caller then re-baselines the pin and re-exposes the tool).
// Returns the removed entry; found=false with no error when the entry did
// not exist (release is idempotent — but a corrupt store is still an error,
// never "already released").
func (s *QuarantineStore) Release(ctx context.Context, exposed string) (QuarantineEntry, bool, error) {
	var (
		out   QuarantineEntry
		found bool
	)
	err := s.f.withLock(ctx, func() error {
		file, _, err := loadStore[quarantineFile](s.f.path, func(v *quarantineFile) int { return v.Version })
		if err != nil {
			return err
		}
		out, found = file.Entries[exposed]
		if !found {
			return nil
		}
		delete(file.Entries, exposed)
		file.Version = storeVersion
		return s.f.save(file)
	})
	if err != nil {
		return QuarantineEntry{}, false, err
	}
	return out, found, nil
}

// Snapshot returns the full quarantine set (exposed name -> entry).
func (s *QuarantineStore) Snapshot(ctx context.Context) (map[string]QuarantineEntry, error) {
	var out map[string]QuarantineEntry
	err := s.f.withLock(ctx, func() error {
		file, _, err := loadStore[quarantineFile](s.f.path, func(v *quarantineFile) int { return v.Version })
		if err != nil {
			return err
		}
		out = file.Entries
		if out == nil {
			out = map[string]QuarantineEntry{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// IsQuarantined reports whether exposed is quarantined.
// Fail direction: on error (corrupt store, lock timeout) the boolean is
// false but the error is non-nil — callers must treat any error as
// "quarantined/blocked" (fail-closed), never consult the boolean alone.
func (s *QuarantineStore) IsQuarantined(ctx context.Context, exposed string) (bool, error) {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	_, ok := snap[exposed]
	return ok, nil
}
