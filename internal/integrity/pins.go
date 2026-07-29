package integrity

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// Pin is one tool's recorded baseline.
type Pin struct {
	// Hash is the pinned fingerprint ("<version>:<hex>").
	Hash string `json:"hash"`
	// HashSchemaVersion records the formula that produced Hash so a formula
	// change is bridged by content comparison, never by a fake rug-pull.
	HashSchemaVersion string `json:"hashSchemaVersion"`
	// Snapshot is the content behind Hash — it powers diff review and the
	// formula-migration content check.
	Snapshot ToolSnapshot `json:"snapshot"`

	FirstSeen   time.Time `json:"firstSeen"`
	LastChanged time.Time `json:"lastChanged"`
}

// pinsFile is the on-disk envelope of state/tool-pins.json:
// server ID -> raw tool name -> Pin.
type pinsFile struct {
	Version int                       `json:"version"`
	Pins    map[string]map[string]Pin `json:"pins"`
}

// PinStore persists tool fingerprint baselines in <state>/tool-pins.json,
// guarded by a sibling cross-process lock. Safe for concurrent use from
// multiple goroutines and multiple processes.
type PinStore struct {
	f *lockedFile
}

// OpenPinStore opens (creating the directory if needed) the pin store under
// stateDir — normally platform.StateDir().
func OpenPinStore(stateDir string, opts Options) (*PinStore, error) {
	f, err := newLockedFile(stateDir, pinsFileName, opts)
	if err != nil {
		return nil, err
	}
	return &PinStore{f: f}, nil
}

// CheckServer fingerprints the freshly observed catalog of one server
// against the stored pins and returns the classified drift, sorted by tool
// name (determinism is contract — output feeds golden-tested audit lines).
//
// Persistence semantics (docs/flows.md):
//   - New tools are pinned immediately (baseline) — and never quarantined.
//   - Changed tools keep their old pin: re-baselining happens only through
//     Rebaseline after an explicit user release/approve.
//   - Removed tools keep their pin: merge never deletes. A pin is discarded
//     only by ForgetServer, i.e. when the server itself is removed.
//   - A pin recorded under an older hash formula whose content is unchanged
//     is migrated in place and reported Unchanged.
//
// Fail direction: any storage error (ErrStoreCorrupt included) or
// un-fingerprintable tool aborts the whole check with an error and writes
// nothing — a corrupt store is never overwritten and never treated as empty
// (fail-closed).
func (s *PinStore) CheckServer(ctx context.Context, server string, tools []ToolSnapshot) ([]Drift, error) {
	var drifts []Drift
	err := s.f.withLock(ctx, func() error {
		file, _, err := loadStore[pinsFile](s.f.path, func(v *pinsFile) int { return v.Version })
		if err != nil {
			return err
		}
		if file.Pins == nil {
			file.Pins = map[string]map[string]Pin{}
		}
		srvPins := file.Pins[server]
		if srvPins == nil {
			srvPins = map[string]Pin{}
		}

		now := s.f.now().UTC()
		dirty := false
		seen := make(map[string]bool, len(tools))
		for _, t := range tools {
			if seen[t.Name] {
				return fmt.Errorf("integrity: server %q lists tool %q twice", server, t.Name)
			}
			seen[t.Name] = true
			fp, err := Fingerprint(t)
			if err != nil {
				return err
			}
			pin, ok := srvPins[t.Name]
			switch {
			case !ok:
				srvPins[t.Name] = Pin{
					Hash:              fp,
					HashSchemaVersion: HashSchemaVersion,
					Snapshot:          t,
					FirstSeen:         now,
					LastChanged:       now,
				}
				dirty = true
				drifts = append(drifts, Drift{
					Server: server, Tool: t.Name, Kind: DriftNew,
					CurrentHash: fp, Current: t,
				})
			case pin.HashSchemaVersion == HashSchemaVersion && pin.Hash == fp:
				drifts = append(drifts, Drift{
					Server: server, Tool: t.Name, Kind: DriftUnchanged,
					PinnedHash: pin.Hash, CurrentHash: fp, Pinned: pin.Snapshot, Current: t,
				})
			case pin.HashSchemaVersion != HashSchemaVersion:
				// Formula migration: recompute the PINNED snapshot under the
				// current formula. Identical content must migrate silently —
				// a formula change may never present as a rug-pull.
				refp, rerr := Fingerprint(pin.Snapshot)
				if rerr == nil && refp == fp {
					pin.Hash = fp
					pin.HashSchemaVersion = HashSchemaVersion
					srvPins[t.Name] = pin
					dirty = true
					drifts = append(drifts, Drift{
						Server: server, Tool: t.Name, Kind: DriftUnchanged,
						PinnedHash: fp, CurrentHash: fp, Pinned: pin.Snapshot, Current: t,
					})
					continue
				}
				drifts = append(drifts, changedDrift(server, t, pin, fp))
			default:
				drifts = append(drifts, changedDrift(server, t, pin, fp))
			}
		}

		// Pinned tools absent from the catalog: report Removed, keep the pin.
		for name, pin := range srvPins {
			if !seen[name] {
				drifts = append(drifts, Drift{
					Server: server, Tool: name, Kind: DriftRemoved,
					PinnedHash: pin.Hash, Pinned: pin.Snapshot,
				})
			}
		}

		slices.SortFunc(drifts, func(a, b Drift) int { return cmp.Compare(a.Tool, b.Tool) })

		if !dirty {
			return nil
		}
		file.Version = storeVersion
		file.Pins[server] = srvPins
		return s.f.save(file)
	})
	if err != nil {
		return nil, err
	}
	return drifts, nil
}

// changedDrift builds a DriftChanged entry, attributing the change to
// description and/or schema by comparing snapshots.
func changedDrift(server string, cur ToolSnapshot, pin Pin, fp string) Drift {
	return Drift{
		Server: server, Tool: cur.Name, Kind: DriftChanged,
		PinnedHash: pin.Hash, CurrentHash: fp,
		DescChanged:   cur.Description != pin.Snapshot.Description,
		SchemaChanged: !schemasEqual(cur.InputSchema, pin.Snapshot.InputSchema),
		Pinned:        pin.Snapshot, Current: cur,
	}
}

// ForgetServer deletes every pin of one server — the cleanup half of
// `agenthub server rm`.
//
// This is the one exception to "merge never deletes" in CheckServer, and the
// distinction is what makes both rules safe: merge must not delete because a
// tool vanishing from a catalog is exactly how a rug-pull hides, whereas here
// the SERVER is gone, so there is no catalog left to compare against. Keeping
// the pins would mean a different server re-added under the same id matches
// old baselines and is classified Unchanged — drift detection silently
// disarmed for tools it never presented before.
//
// A server with no pins is a no-op (StateForgetter contract). Fail direction
// follows the rest of the store: a corrupt file aborts and is never
// overwritten.
func (s *PinStore) ForgetServer(ctx context.Context, server string) error {
	return s.f.withLock(ctx, func() error {
		file, found, err := loadStore[pinsFile](s.f.path, func(v *pinsFile) int { return v.Version })
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if _, ok := file.Pins[server]; !ok {
			return nil
		}
		delete(file.Pins, server)
		file.Version = storeVersion
		return s.f.save(file)
	})
}

// StateName implements confops.StateForgetter.
func (s *PinStore) StateName() string { return "tool pins" }

// Rebaseline moves one tool's pin forward to snap — the re-approve step
// after a user reviewed a drift (quarantine release, docs/flows.md). Merge
// semantics: FirstSeen is preserved when the pin exists, LastChanged always
// advances; a missing pin is created (never an error — release must not
// race-fail against a fresh store).
func (s *PinStore) Rebaseline(ctx context.Context, server, tool string, snap ToolSnapshot) (Pin, error) {
	if snap.Name != tool {
		return Pin{}, fmt.Errorf("integrity: rebaseline %s/%s: snapshot names %q", server, tool, snap.Name)
	}
	fp, err := Fingerprint(snap)
	if err != nil {
		return Pin{}, err
	}
	var out Pin
	err = s.f.withLock(ctx, func() error {
		file, _, err := loadStore[pinsFile](s.f.path, func(v *pinsFile) int { return v.Version })
		if err != nil {
			return err
		}
		if file.Pins == nil {
			file.Pins = map[string]map[string]Pin{}
		}
		if file.Pins[server] == nil {
			file.Pins[server] = map[string]Pin{}
		}
		now := s.f.now().UTC()
		pin, ok := file.Pins[server][tool]
		if !ok {
			pin.FirstSeen = now
		}
		pin.Hash = fp
		pin.HashSchemaVersion = HashSchemaVersion
		pin.Snapshot = snap
		pin.LastChanged = now
		file.Pins[server][tool] = pin
		file.Version = storeVersion
		out = pin
		return s.f.save(file)
	})
	if err != nil {
		return Pin{}, err
	}
	return out, nil
}

// Pins returns a snapshot of all stored pins (server -> tool -> Pin).
// Fail direction: a corrupt store returns ErrStoreCorrupt, never an empty
// map (fail-closed).
func (s *PinStore) Pins(ctx context.Context) (map[string]map[string]Pin, error) {
	var out map[string]map[string]Pin
	err := s.f.withLock(ctx, func() error {
		file, _, err := loadStore[pinsFile](s.f.path, func(v *pinsFile) int { return v.Version })
		if err != nil {
			return err
		}
		out = file.Pins
		if out == nil {
			out = map[string]map[string]Pin{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
