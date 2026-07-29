package confops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// ProfileResult is what the profile operations return.
type ProfileResult struct {
	Result
	// Name is the profile the operation ended on ("" after a clear).
	Name string
	// OldName is set by RenameProfile.
	OldName string
	// Profile is the entry as it now stands; Exists says whether it still
	// does (false after RemoveProfile).
	Profile registry.Profile
	Exists  bool
	// Repointed lists the client ids whose profile reference a rename
	// rewrote.
	Repointed []string
	// Dangling lists the client ids left pointing at a removed profile.
	// They are NOT rewritten (fail-closed); they are reported so the
	// operator is never surprised silently.
	Dangling []string
	// ActiveCleared reports that the removed profile was the active one and
	// the active marker was cleared with it.
	ActiveCleared bool
}

// CreateProfile adds an empty (or pre-populated) profile.
//
// servers keeps the three-state distinction: nil means "no narrowing at all"
// — a fresh profile sees every registered server — while an empty slice
// means block-all. Refusing to collapse the two is what keeps a fresh
// profile from silently becoming a deny-everything one, and vice versa.
func CreateProfile(
	ctx context.Context, st *registry.Store, name string, servers []string, pre Precondition,
) (ProfileResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProfileResult{}, usagef("profile name must not be empty")
	}
	var p registry.Profile
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if tx.Profiles.V.Profiles == nil {
			tx.Profiles.V.Profiles = map[string]registry.Doc[registry.Profile]{}
		}
		if _, exists := tx.Profiles.V.Profiles[name]; exists {
			e := conflictf(CodeProfileExists, "profile %q already exists", name)
			e.Hint = "edit it with 'agenthub profile server add' / 'agenthub profile tools'"
			return e
		}
		p = registry.Profile{Servers: dedupSorted(servers)}
		tx.Profiles.V.Profiles[name] = registry.Doc[registry.Profile]{V: p}
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	return ProfileResult{Result: res, Name: name, Profile: p, Exists: true}, nil
}

// RenameProfile renames a profile AND repoints every client and project
// binding that names it, in both the shorthand and the explicit ProfileRef
// form, plus the active-profile marker.
//
// The repointing is the whole reason this is an operation rather than a
// delete-then-create: leaving the references behind would fail-close every
// one of those clients to an empty scope (docs/architecture.md §7), which is a
// silent, total loss of tool access. The active-profile marker is repointed
// with them, in the same transaction — it lives in the same document set.
func RenameProfile(
	ctx context.Context, st *registry.Store, oldName, newName string, pre Precondition,
) (ProfileResult, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return ProfileResult{}, usagef("the new profile name must not be empty")
	}
	if oldName == newName {
		return ProfileResult{}, usagef("the new profile name is the same as the old one")
	}
	var (
		repointed []string
		p         registry.Profile
	)
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Profiles.V.Profiles[oldName]
		if !ok {
			return profileNotFound(oldName)
		}
		if _, exists := tx.Profiles.V.Profiles[newName]; exists {
			return conflictf(CodeProfileExists, "profile %q already exists", newName)
		}
		delete(tx.Profiles.V.Profiles, oldName)
		tx.Profiles.V.Profiles[newName] = doc
		repointed = repointProfileRefs(tx, oldName, newName)
		// The active marker is a profile reference like any other and now
		// lives in the same document set, so it is repointed INSIDE the
		// transaction. Read-then-write across two stores could leave the
		// marker naming a profile the rename already removed, which resolves
		// fail-closed for every client that follows it.
		if tx.Governance.V.ActiveProfile == oldName {
			tx.Governance.V.ActiveProfile = newName
		}
		p = doc.V
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	return ProfileResult{
		Result: res, Name: newName, OldName: oldName,
		Profile: p, Exists: true, Repointed: repointed,
	}, nil
}

// repointProfileRefs rewrites every client binding that names oldName and
// returns the client ids it touched, in ascending id order.
func repointProfileRefs(tx *registry.Tx, oldName, newName string) []string {
	var touched []string
	for _, id := range sortedKeys(tx.Clients.V.Clients) {
		doc := tx.Clients.V.Clients[id]
		entry := doc.V
		changed := false
		if entry.Profile == oldName {
			entry.Profile = newName
			changed = true
		}
		if entry.ProfileRef != nil && entry.ProfileRef.V.Kind == registry.BindingNamed &&
			entry.ProfileRef.V.Name == oldName {
			ref := *entry.ProfileRef
			ref.V.Name = newName
			entry.ProfileRef = &ref
			changed = true
		}
		if changed {
			doc.V = entry
			tx.Clients.V.Clients[id] = doc
			touched = append(touched, id)
		}
	}
	return touched
}

// RemoveProfile deletes a profile.
//
// Referencing clients are deliberately NOT rewritten: a dangling reference
// resolves to an EMPTY scope, never a widened one (docs/architecture.md §7). They are
// reported — in Dangling and as warnings — because "your client just lost
// every tool" is not something an operator may learn by accident.
func RemoveProfile(
	ctx context.Context, st *registry.Store, name string, pre Precondition,
) (ProfileResult, error) {
	var dangling []string
	var activeCleared bool
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		if _, ok := tx.Profiles.V.Profiles[name]; !ok {
			return profileNotFound(name)
		}
		delete(tx.Profiles.V.Profiles, name)
		dangling = nil
		for _, id := range sortedKeys(tx.Clients.V.Clients) {
			if tx.Clients.V.Clients[id].V.Binding().Name == name {
				dangling = append(dangling, id)
			}
		}
		// Clearing the marker in the SAME transaction that removes the
		// profile is what keeps it from ever naming something deleted. Unlike
		// a client reference — deliberately left dangling and reported — the
		// marker is the fallback for every client that names no profile, so
		// leaving it would fail-close all of them at once.
		activeCleared = tx.Governance.V.ActiveProfile == name
		if activeCleared {
			tx.Governance.V.ActiveProfile = ""
		}
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	out := ProfileResult{Result: res, Name: name, Dangling: dangling}
	for _, id := range dangling {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"client %q still references profile %q; it now resolves to an EMPTY scope (fail-closed)", id, name))
	}
	if activeCleared {
		out.ActiveCleared = true
		out.Warnings = append(out.Warnings, "the removed profile was active; active profile cleared")
	}
	return out, nil
}

// SetProfileServers edits a profile's three-state server set.
//
// The read-modify-write happens INSIDE the lock, so add/remove cannot lose a
// concurrent edit the way a caller computing the new list from a stale
// snapshot would.
func SetProfileServers(
	ctx context.Context, st *registry.Store, profile string, sel ServerSelection, pre Precondition,
) (ProfileResult, error) {
	var p registry.Profile
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Profiles.V.Profiles[profile]
		if !ok {
			return profileNotFound(profile)
		}
		switch sel.Mode {
		case ServerSetReplace:
			for _, id := range sel.Servers {
				if _, ok := tx.Servers.V.Servers[id]; !ok {
					return serverNotFound(id)
				}
			}
			doc.V.Servers = dedupSorted(sel.Servers)
		case ServerSetAdd:
			if len(sel.Servers) == 0 {
				return usagef("no server to add")
			}
			for _, id := range sel.Servers {
				if _, ok := tx.Servers.V.Servers[id]; !ok {
					return serverNotFound(id)
				}
			}
			list := doc.V.Servers
			if list == nil {
				list = []string{}
			}
			doc.V.Servers = dedupSorted(append(list, sel.Servers...))
		case ServerSetRemove:
			if len(sel.Servers) == 0 {
				return usagef("no server to remove")
			}
			if doc.V.Servers == nil {
				return notFoundf(CodeNotFound,
					"profile %q has no explicit server set (it sees every registered server)", profile)
			}
			for _, id := range sel.Servers {
				before := len(doc.V.Servers)
				doc.V.Servers = slices.DeleteFunc(doc.V.Servers, func(s string) bool { return s == id })
				if len(doc.V.Servers) == before {
					return notFoundf(CodeServerNotFound, "profile %q does not list server %q", profile, id)
				}
			}
		default:
			return usagef("a server-set edit needs a mode: replace, add or remove")
		}
		tx.Profiles.V.Profiles[profile] = doc
		p = doc.V
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	return ProfileResult{Result: res, Name: profile, Profile: p, Exists: true}, nil
}

// SetProfileTools sets a profile's three-state tool selector for one server.
//
// Both the profile and the server must exist: a selector naming a server the
// registry does not know narrows nothing, so accepting it would be a rule
// that silently does not apply.
func SetProfileTools(
	ctx context.Context, st *registry.Store, profile, server string, sel ToolSelection, pre Precondition,
) (ProfileResult, error) {
	if err := sel.validate(); err != nil {
		return ProfileResult{}, err
	}
	var p registry.Profile
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Profiles.V.Profiles[profile]
		if !ok {
			return profileNotFound(profile)
		}
		if _, ok := tx.Servers.V.Servers[server]; !ok {
			return serverNotFound(server)
		}
		if doc.V.Tools == nil {
			doc.V.Tools = map[string]registry.Doc[registry.ToolSelector]{}
		}
		applySelector(doc.V.Tools, server, sel)
		if len(doc.V.Tools) == 0 {
			doc.V.Tools = nil
		}
		tx.Profiles.V.Profiles[profile] = doc
		p = doc.V
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	return ProfileResult{Result: res, Name: profile, Profile: p, Exists: true}, nil
}

// SetProfileDiscovery sets how a profile's tools are surfaced ("lazy",
// "grouped", "full"), or clears the override when mode is "" so the global
// default applies again.
//
// Discovery is an experience field, not a security one: it changes how the
// same tool set is presented, never which tools are in it. That is why an
// unknown mode is refused here rather than left to the resolver, which would
// silently fall back to a default the operator did not choose.
func SetProfileDiscovery(
	ctx context.Context, st *registry.Store, profile, mode string, pre Precondition,
) (ProfileResult, error) {
	if mode != "" {
		if err := ValidateDiscovery(mode); err != nil {
			return ProfileResult{}, err
		}
	}
	var p registry.Profile
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		doc, ok := tx.Profiles.V.Profiles[profile]
		if !ok {
			return profileNotFound(profile)
		}
		doc.V.Discovery = mode
		tx.Profiles.V.Profiles[profile] = doc
		p = doc.V
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res}, err
	}
	return ProfileResult{Result: res, Name: profile, Profile: p, Exists: true}, nil
}

// SetActiveProfile points the global active marker at a profile, or clears
// it when name is "" (every registered server visible again).
//
// A named profile must exist. The marker is the fallback for every client
// that does not name one itself, so pointing it at a typo would fail-close
// all of them at once.
func SetActiveProfile(
	ctx context.Context, st *registry.Store, name string, pre Precondition,
) (ProfileResult, error) {
	if st == nil {
		// Setting the marker means writing the registry, and a named profile
		// must be resolved against it: a typo would otherwise fail-close
		// every client that follows the marker.
		return ProfileResult{}, usagef("no registry store for the active-profile marker")
	}
	var previous string
	var profile registry.Profile
	var exists bool
	res, err := apply(ctx, st, pre, func(tx *registry.Tx) error {
		previous = tx.Governance.V.ActiveProfile
		if name != "" {
			doc, ok := tx.Profiles.V.Profiles[name]
			if !ok {
				return profileNotFound(name)
			}
			profile, exists = doc.V, true
		}
		tx.Governance.V.ActiveProfile = name
		return nil
	})
	if err != nil {
		return ProfileResult{Result: res, Name: name}, err
	}
	return ProfileResult{
		Result: res, Name: name, OldName: previous,
		Profile: profile, Exists: exists,
	}, nil
}

// profileNotFound is the shared "no such profile" refusal.
func profileNotFound(name string) *Error {
	e := notFoundf(CodeProfileNotFound, "no profile %q", name)
	e.Hint = "run 'agenthub profile ls' to see configured profiles"
	return e
}

// ActiveProfileFileName is the PRE-MIGRATION home of the active profile
// name: <state>/active-profile.json.
//
// docs/architecture.md §7 puts `activeProfile` in the GLOBAL governance layer, and it
// now lives there (registry.GovernanceDoc.ActiveProfile). The state file was
// a stand-in while the schema lacked the field — but scope resolution is
// pure and reads no files, so a marker kept outside the registry could be
// set and listed while no session ever applied it. The name is kept only so
// MigrateActiveProfile can retire the old file.
const ActiveProfileFileName = "active-profile.json"

type activeProfileFile struct {
	Version int    `json:"version"`
	Profile string `json:"profile"`
}

// ActiveProfile reads the globally active profile name from the registry.
//
// Failure direction: no snapshot reads as "no active profile" — the same
// direction a dangling reference takes. The failure mode of an unreadable
// marker must be "no narrowing source", never "some arbitrary profile".
func ActiveProfile(st *registry.Store) (string, error) {
	if st == nil {
		return "", nil
	}
	return st.Snapshot().Governance.V.ActiveProfile, nil
}

// MigrateActiveProfile moves a pre-migration <state>/active-profile.json
// marker into the registry, then removes the file.
//
// It runs before the marker is first read. Without it an operator who had
// already run `agenthub profile use X` would silently lose that narrowing on
// upgrade — and losing a narrowing WIDENS what every following client sees,
// the one direction this codebase does not accept silently.
//
// A registry value already present wins: it is the newer home, so a stale
// file must never overwrite it. Failure to remove the file is not an error —
// the value is safely migrated by then, and the next run re-reads a marker
// that now loses to the registry anyway.
func MigrateActiveProfile(ctx context.Context, st *registry.Store, stateDir string) (bool, error) {
	if st == nil || stateDir == "" {
		return false, nil
	}
	path := filepath.Join(stateDir, ActiveProfileFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var doc activeProfileFile
	if json.Unmarshal(b, &doc) != nil || doc.Profile == "" {
		// Unparseable or already cleared: nothing to carry over. Drop the
		// file so the check does not repeat every start.
		_ = os.Remove(path)
		return false, nil
	}
	var moved bool
	// No precondition: the migration is not racing an operator's edit, and a
	// generation check would only turn a concurrent unrelated write into a
	// lost marker.
	_, err = apply(ctx, st, Precondition{}, func(tx *registry.Tx) error {
		if tx.Governance.V.ActiveProfile != "" {
			return nil // the registry already holds a marker; it wins
		}
		tx.Governance.V.ActiveProfile = doc.Profile
		moved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	_ = os.Remove(path)
	return moved, nil
}

// atomicWriteJSON marshals v and replaces path by rename, so a reader never
// observes a half-written document (the registry's write discipline, in
// miniature, for the small state files).
func atomicWriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
