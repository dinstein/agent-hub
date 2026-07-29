package approval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// AllowlistFileName under <state>/ (platform.StateDir). Frozen: renaming it
// would orphan every existing remember-forever grant.
const AllowlistFileName = "approvals-allowlist.json"

// allowlistVersion is the on-disk envelope version.
const allowlistVersion = 1

// Entry is one remembered approval, keyed by tool fingerprint. Server, Tool
// and ArgsHash are optional extra bindings: when set they must ALSO match.
// The fingerprint already covers name/description/schema of one tool, so the
// server/tool binding is defense in depth against fingerprint collisions
// across servers; the ArgsHash binding narrows a grant to one exact argument
// payload. No entry ever stores argument bytes — hashes only.
type Entry struct {
	Fingerprint string     `json:"fingerprint"`
	Server      string     `json:"server,omitempty"`
	Tool        string     `json:"tool,omitempty"`
	ArgsHash    string     `json:"argsHash,omitempty"`
	GateReason  GateReason `json:"gateReason,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// matches reports whether e covers req. Fail direction: any mismatch —
// including an empty fingerprint on either side — is a miss, and a miss
// sends the call to a human. Never widens.
func (e Entry) matches(req Request) bool {
	if e.Fingerprint == "" || req.Fingerprint == "" || e.Fingerprint != req.Fingerprint {
		return false
	}
	if e.Server != "" && e.Server != req.Server {
		return false
	}
	if e.Tool != "" && e.Tool != req.Tool {
		return false
	}
	if e.ArgsHash != "" && e.ArgsHash != req.ArgsHash {
		return false
	}
	return true
}

type allowlistFile struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Allowlist is the on-disk remember-forever store. The daemon is the single
// writer, so there is no cross-process lock — only an
// in-process mutex plus the atomic-write ladder. Entries are kept in memory;
// every mutation rewrites the whole file atomically.
type Allowlist struct {
	mu      sync.Mutex
	path    string
	entries map[string]Entry // key = fingerprint
}

// OpenAllowlist loads (or initializes) <stateDir>/approvals-allowlist.json.
//
// A missing file is an empty allowlist. A corrupt or future-versioned file
// is an error and the file is left untouched (fail-closed both ways: no
// entry is trusted from a file we cannot fully parse, and we never overwrite
// evidence). The caller then runs without an allowlist — every gated call
// goes to a human, which is the safe direction.
func OpenAllowlist(stateDir string) (*Allowlist, error) {
	if stateDir == "" {
		return nil, errors.New("approval: state dir must not be empty")
	}
	// EnsureDir enforces 0700: grants are security state, never group/world
	// readable.
	if err := platform.EnsureDir(stateDir); err != nil {
		return nil, err
	}
	a := &Allowlist{
		path:    filepath.Join(stateDir, AllowlistFileName),
		entries: map[string]Entry{},
	}
	raw, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, fmt.Errorf("approval: allowlist: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		// Atomic writers never leave an empty file; treat as corrupt.
		return nil, fmt.Errorf("approval: allowlist %s: file is empty", a.path)
	}
	var f allowlistFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("approval: allowlist %s: %w", a.path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("approval: allowlist %s: trailing data after JSON document", a.path)
	}
	if f.Version != allowlistVersion {
		// A future version may carry binding fields this binary does not
		// understand; honoring only the fields we know could WIDEN a grant.
		return nil, fmt.Errorf("approval: allowlist %s: unsupported version %d (want %d)", a.path, f.Version, allowlistVersion)
	}
	if f.Entries != nil {
		a.entries = f.Entries
	}
	return a, nil
}

// Match reports whether req is covered by a stored grant. Miss on any doubt.
func (a *Allowlist) Match(req Request) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[req.Fingerprint]
	return ok && e.matches(req)
}

// Add stores (or replaces) the grant for e.Fingerprint and persists
// atomically. An entry without a fingerprint is refused: it could never
// match anything and would only bloat the file.
func (a *Allowlist) Add(e Entry) error {
	if e.Fingerprint == "" {
		return errors.New("approval: allowlist entry needs a fingerprint")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, existed := a.entries[e.Fingerprint]
	a.entries[e.Fingerprint] = e
	if err := a.save(); err != nil {
		// Keep memory and disk consistent: roll back so a later save cannot
		// silently resurrect an entry the caller was told failed.
		if existed {
			a.entries[e.Fingerprint] = prev
		} else {
			delete(a.entries, e.Fingerprint)
		}
		return err
	}
	return nil
}

// Remove deletes the grant for fingerprint. Returns whether it existed.
func (a *Allowlist) Remove(fingerprint string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, ok := a.entries[fingerprint]
	if !ok {
		return false, nil
	}
	delete(a.entries, fingerprint)
	if err := a.save(); err != nil {
		a.entries[fingerprint] = prev
		return false, err
	}
	return true, nil
}

// Entries returns a snapshot copy (for `agenthub approval ls`).
func (a *Allowlist) Entries() []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Entry, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e)
	}
	return out
}

// save persists the current entry set. Caller holds a.mu.
func (a *Allowlist) save() error {
	b, err := json.MarshalIndent(allowlistFile{Version: allowlistVersion, Entries: a.entries}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(a.path, append(b, '\n'))
}

// atomicWrite persists data to path: same-directory temp file, chmod 0600,
// write, fsync, rename over the target, fsync of the parent directory.
// Never leaves a partially written target. (Independent copy of the
// registry/integrity ladder — those packages do not export it, and approval
// must not depend on them.)
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a preceding rename is durable. Filesystems
// that do not support fsync on directories are tolerated — the rename itself
// is still atomic there.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}
