package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// HashSchemaVersion prefixes every skill fingerprint ("v1:<hex>") and is
// stored next to each pin.
//
// Why the prefix, restated for this package: when the formula changes, a
// pin recorded under the old one must be recognizable as "computed
// differently", not as "changed content". Without the prefix a formula bump
// presents as a fleet-wide tamper alert, and users learn to click through
// tamper alerts.
//
// Deliberately separate from integrity.HashSchemaVersion: skills fingerprint
// a different snapshot type (files + package metadata, not a tool
// definition), so the two formulas must be free to version independently.
const HashSchemaVersion = "v1"

// Snapshot is the fingerprint input: everything whose change should count
// as "this skill is not what it was".
//
// Files carries content hashes, so the fingerprint covers every byte of the
// package; Name/Description/Kind are included because they are what a
// client's model actually reads when deciding to invoke the skill — a
// description swap with identical files IS a meaningful change (and is the
// classic prompt-injection vector, docs/modules/config.md).
//
// Deliberately absent: Version and timestamps. A version bump with
// identical content is not a content change, and including timestamps would
// make the fingerprint unstable across re-imports of the same bytes.
type Snapshot struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Kind        SkillKind   `json:"kind"`
	Files       []FileEntry `json:"files"`
}

// SnapshotOf builds the fingerprint input of a library entry.
func SnapshotOf(s *Skill) Snapshot {
	return Snapshot{Name: s.Name, Description: s.Description, Kind: s.Kind, Files: s.Files}
}

// Fingerprint computes "v1:" + hex(sha256(canonical(snapshot))).
//
// Canonicalization: the file list is sorted by path (import order and
// filesystem readdir order must never move the fingerprint) and the whole
// snapshot is marshaled through encoding/json, whose map-free struct
// encoding is already deterministic.
//
// Fail direction: a snapshot that cannot be marshaled returns an error and
// the caller must treat the skill as un-fingerprintable — never as
// "matches its pin".
func Fingerprint(s Snapshot) (string, error) {
	files := make([]FileEntry, len(s.Files))
	copy(files, s.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	s.Files = files
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("skills: fingerprint %q: %w", s.Name, err)
	}
	sum := sha256.Sum256(payload)
	return HashSchemaVersion + ":" + hex.EncodeToString(sum[:]), nil
}

// ContentHash is the hash over file paths, sizes and content hashes only.
// It is the CAS directory name and the Stale/Drift comparison key.
// Narrower than Fingerprint on purpose: two library versions differing only
// in description share no ContentHash requirement, but they must not share
// a fingerprint.
func ContentHash(files []FileEntry) string {
	sorted := make([]FileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		// hash.Hash writes never fail; the error is discarded deliberately.
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00", f.Path, f.SHA256, f.Size)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashBytes is the content hash of a single blob (file body, sentinel block
// body). Plain hex sha256, no version prefix: unlike Fingerprint this is
// never persisted as a trust baseline, only compared against a value this
// same binary wrote.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
