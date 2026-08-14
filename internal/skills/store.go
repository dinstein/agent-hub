package skills

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/platform"
)

// File and directory names under the skills directory. Frozen: renaming any
// of them orphans every existing library entry and receipt.
const (
	skillsFileName   = "skills.json"
	installsFileName = "installs.json"
	pinsFileName     = "skill-pins.json"
	storeDirName     = "store"
	backupsDirName   = "backups"
	lockFileName     = ".lock"

	// MarkerFileName proves an installed directory is ours. Its presence is
	// the ONLY thing that lets Apply/Remove touch a directory
	// (docs/subsystems/skills.md: OwnedFile's frozen prefix, in directory form).
	MarkerFileName = ".agenthub-managed.json"
)

const (
	// storeVersion is the on-disk envelope version of all three files.
	storeVersion = 1

	defaultLockTimeout = 10 * time.Second

	// readRetries absorbs rename transients from a lock-free reader path;
	// after them a parse failure is a genuine corruption.
	readRetries = 4

	// keptVersions is how many content-addressed library versions per skill
	// survive a prune (docs/subsystems/skills.md: "gc keeps the 3 most recent versions"). Old versions
	// are what a rollback and a drift diff read from.
	keptVersions = 3
)

// readRetryDelay is a variable only so corruption tests need not pay the
// full ladder; production code never mutates it.
var readRetryDelay = 75 * time.Millisecond

// Options tunes a Manager. The zero value is usable.
type Options struct {
	// LockTimeout bounds cross-process lock acquisition (default 10s).
	LockTimeout time.Duration
	// Now overrides the clock (tests). Default time.Now.
	Now func() time.Time
	// HomeDir overrides the user home used by user-scope target
	// conventions. Empty means os.UserHomeDir.
	HomeDir string
	// ExtraTargets are added to (and override, by ClientID) the built-in
	// target table. Used by tests and by future user-defined targets.
	ExtraTargets []TargetDef
	// BackupDir is where a shared file is copied before agenthub edits it.
	// Default <skills>/backups; the daemon passes <data>/backups/skills.
	BackupDir string
	// AgentVersion is recorded in every owned-dir marker so an operator can
	// tell which build wrote a directory.
	AgentVersion string
	// ContentScanner, when set, is called on import and update with the
	// package's untrusted text (field is "name", "description" or a file
	// path). Returning an error REFUSES the import. It is a SEAM and nothing
	// sets it today: the injection scanner it was shaped for went with the
	// removed governance surface. Kept because SKILL.md is a first-class
	// prompt-injection carrier and the refusal has to happen at import, and
	// kept as a hook so this package stays free of guard dependencies.
	ContentScanner func(field, text string) error
	// MaxFileSize, MaxTotalSize and MaxFiles bound an import. Zero values
	// use the defaults below.
	MaxFileSize  int64
	MaxTotalSize int64
	MaxFiles     int
}

// Import bounds. Generous enough for real skill packages, small enough that
// a mistyped path (a home directory, a repository with node_modules) fails
// fast instead of copying gigabytes into the store.
const (
	defaultMaxFileSize  = 4 << 20  // 4 MiB
	defaultMaxTotalSize = 32 << 20 // 32 MiB
	defaultMaxFiles     = 2000
)

func (o *Options) applyDefaults(dir string) {
	if o.LockTimeout <= 0 {
		o.LockTimeout = defaultLockTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.BackupDir == "" {
		o.BackupDir = filepath.Join(dir, backupsDirName)
	}
	if o.MaxFileSize <= 0 {
		o.MaxFileSize = defaultMaxFileSize
	}
	if o.MaxTotalSize <= 0 {
		o.MaxTotalSize = defaultMaxTotalSize
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = defaultMaxFiles
	}
}

// On-disk envelopes.

type skillsFile struct {
	Version int              `json:"version"`
	Skills  map[string]Skill `json:"skills"`
}

type installsFile struct {
	Version int `json:"version"`
	// Installs is a list, not a map: a receipt's identity contains
	// filesystem paths, and no JSON key encoding of a path is both readable
	// and collision-free.
	Installs []InstallState `json:"installs"`
}

type pinsFile struct {
	Version int            `json:"version"`
	Pins    map[string]Pin `json:"pins"`
}

// state is the whole skills state, loaded and saved as one unit under one
// lock. A single lock for three files instead of three locks: every
// interesting operation touches at least two of them (add writes the index
// and the pin; remove writes the index and the receipts), and one lock
// makes cross-file consistency structural rather than an ordering
// convention nobody can verify.
type state struct {
	skills   skillsFile
	installs installsFile
	pins     pinsFile

	// raw* hold the bytes each file was loaded from, powering the no-op
	// guard: an unchanged file is not rewritten, so a read-only operation
	// never bumps an mtime or wakes a watcher.
	rawSkills   []byte
	rawInstalls []byte
	rawPins     []byte
}

func (st *state) skill(id string) (Skill, bool) {
	s, ok := st.skills.Skills[id]
	return s, ok
}

func (st *state) putSkill(s Skill) {
	if st.skills.Skills == nil {
		st.skills.Skills = map[string]Skill{}
	}
	st.skills.Skills[s.ID] = s
}

// sortedSkills returns the library sorted by ID (determinism is contract:
// this output feeds golden-tested CLI rendering).
func (st *state) sortedSkills() []Skill {
	out := make([]Skill, 0, len(st.skills.Skills))
	for _, s := range st.skills.Skills {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Skill) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func (st *state) install(k installKey) (InstallState, int, bool) {
	for i, in := range st.installs.Installs {
		if in.key() == k {
			return in, i, true
		}
	}
	return InstallState{}, -1, false
}

func (st *state) putInstall(in InstallState) {
	if _, i, ok := st.install(in.key()); ok {
		st.installs.Installs[i] = in
		return
	}
	st.installs.Installs = append(st.installs.Installs, in)
}

func (st *state) deleteInstall(k installKey) {
	if _, i, ok := st.install(k); ok {
		st.installs.Installs = append(st.installs.Installs[:i], st.installs.Installs[i+1:]...)
	}
}

// installsOf returns every receipt for one skill.
func (st *state) installsOf(skillID string) []InstallState {
	var out []InstallState
	for _, in := range st.installs.Installs {
		if in.SkillID == skillID {
			out = append(out, in)
		}
	}
	return out
}

// pin baselines a fingerprint. Merge semantics match integrity's:
// FirstSeen survives and LastChanged advances only on a real change.
// Nothing in this package ever deletes a pin — not even Remove — so a skill
// that is removed and added again is compared against its original baseline
// instead of being re-pinned blind.
func (st *state) pin(id, fp string, now time.Time) {
	if st.pins.Pins == nil {
		st.pins.Pins = map[string]Pin{}
	}
	p, ok := st.pins.Pins[id]
	if !ok {
		p.FirstSeen = now
	}
	if p.Fingerprint != fp {
		p.LastChanged = now
	}
	p.Fingerprint = fp
	p.HashSchemaVersion = HashSchemaVersion
	st.pins.Pins[id] = p
}

// Manager is the package's whole API surface: the library, the receipts and
// the installer, behind one cross-process lock.
type Manager struct {
	dir     string
	opts    Options
	targets map[string]TargetDef
}

// Open prepares the skills directory (creating it 0700) and returns a
// Manager. dir is normally <data>/skills.
func Open(dir string, opts Options) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("skills: directory must not be empty")
	}
	opts.applyDefaults(dir)
	if err := platform.EnsureDirs(dir, filepath.Join(dir, storeDirName)); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, opts: opts, targets: targetTable(opts.ExtraTargets)}
	return m, nil
}

// Dir returns the skills directory.
func (m *Manager) Dir() string { return m.dir }

// SkillPath returns the absolute path of a library entry's canonical copy.
func (m *Manager) SkillPath(s *Skill) string { return filepath.Join(m.dir, filepath.FromSlash(s.Path)) }

func (m *Manager) now() time.Time { return m.opts.Now().UTC() }

// withState runs fn under the cross-process lock with the whole state
// loaded, then writes back exactly the files fn changed.
//
// Invariant: no read-modify-write cycle in this package happens outside
// this function, so N gateways plus the daemon serialize on disk instead of
// racing. Read-only callers take the same exclusive lock — the operations
// are short and correctness beats concurrency here.
func (m *Manager) withState(ctx context.Context, fn func(st *state) error) error {
	lock, err := acquireLock(ctx, filepath.Join(m.dir, lockFileName), m.opts.LockTimeout)
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := m.loadState()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return m.saveState(st)
}

func (m *Manager) loadState() (*state, error) {
	st := &state{}
	var err error
	if st.skills, st.rawSkills, err = loadFile[skillsFile](filepath.Join(m.dir, skillsFileName)); err != nil {
		return nil, err
	}
	if st.installs, st.rawInstalls, err = loadFile[installsFile](filepath.Join(m.dir, installsFileName)); err != nil {
		return nil, err
	}
	if st.pins, st.rawPins, err = loadFile[pinsFile](filepath.Join(m.dir, pinsFileName)); err != nil {
		return nil, err
	}
	return st, nil
}

func (m *Manager) saveState(st *state) error {
	slices.SortFunc(st.installs.Installs, func(a, b InstallState) int {
		return cmp.Or(cmp.Compare(a.ClientID, b.ClientID), cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.Container, b.Container), cmp.Compare(a.SkillID, b.SkillID))
	})
	st.skills.Version = storeVersion
	st.installs.Version = storeVersion
	st.pins.Version = storeVersion
	if err := saveIfChanged(filepath.Join(m.dir, skillsFileName), st.skills, st.rawSkills); err != nil {
		return err
	}
	if err := saveIfChanged(filepath.Join(m.dir, installsFileName), st.installs, st.rawInstalls); err != nil {
		return err
	}
	return saveIfChanged(filepath.Join(m.dir, pinsFileName), st.pins, st.rawPins)
}

// versioned is implemented by every on-disk envelope.
type versioned interface {
	version() int
}

func (f skillsFile) version() int   { return f.Version }
func (f installsFile) version() int { return f.Version }
func (f pinsFile) version() int     { return f.Version }

// loadFile reads one state file.
//
// A MISSING file is a fresh store (zero value, no error) — first run has no
// skills. Anything else — unreadable, unparseable, trailing garbage, empty
// (atomic writers never leave one), unsupported version — is *CorruptError
// and the caller must abort: never an empty set, never renamed aside.
func loadFile[T versioned](path string) (T, []byte, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= readRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(readRetryDelay)
		}
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return zero, nil, nil
		}
		if err != nil {
			lastErr = err
			continue
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			lastErr = errors.New("file is empty (atomic writers never produce empty files)")
			continue
		}
		var v T
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&v); err != nil {
			lastErr = err
			continue
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			lastErr = errors.New("trailing data after JSON document")
			continue
		}
		if got := v.version(); got != storeVersion {
			lastErr = fmt.Errorf("unsupported store version %d (want %d)", got, storeVersion)
			continue
		}
		return v, raw, nil
	}
	return zero, nil, &CorruptError{Path: path, Err: lastErr}
}

// saveIfChanged writes v only when its encoding differs from what was
// loaded (no-op guard).
func saveIfChanged(path string, v any, loaded []byte) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if loaded != nil && bytes.Equal(bytes.TrimSpace(loaded), bytes.TrimSpace(b)) {
		return nil
	}
	return atomicWrite(path, b)
}

// atomicWrite persists data: same-directory temp file, chmod 0600, write,
// fsync, rename over the target, fsync parent. Never leaves a partially
// written target. (Independent copy of the registry/integrity ladder — see
// doc.go for why neither package is imported.)
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := platform.EnsureDir(dir); err != nil {
		return err
	}
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
// without directory fsync are tolerated: the rename is atomic there anyway.
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

// backupFile copies path into the backup directory before agenthub edits a
// file it does not own (docs/subsystems/skills.md: central backup dir, clients.rs'
// discipline). A missing source is not an error — there is nothing to lose.
func (m *Manager) backupFile(clientID, path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(m.opts.BackupDir, clientID)
	if err := platform.EnsureDir(dir); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d-%s", m.now().UnixMilli(), filepath.Base(path))
	dst := filepath.Join(dir, name)
	if err := atomicWrite(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}
