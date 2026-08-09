package skills

import "testing"

// The on-disk names of internal/skills are frozen, and store.go says why:
// "renaming any of them orphans every existing library entry and receipt."
// MarkerFileName says it more sharply still — its presence is the ONLY thing
// that lets Apply or Remove touch an installed directory, so a rename does
// not merely lose a file, it makes every directory installed by an older
// build un-ownable and therefore untouchable.
//
// Nothing enforced that. Every use in the package and in the suite goes
// through the identifier, which moves with the constant: rename the value and
// code and tests agree on the new spelling while the bytes already on a
// user's disk agree on the old one. The literals in the prose of
// installorder_test.go and trust_test.go read like coverage and are comments.
//
// So the values are written out once more, here, where changing one costs a
// deliberate edit to a file whose whole subject is that they do not change.
func TestOnDiskNamesAreFrozen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ got, want, why string }{
		{skillsFileName, "skills.json", "the library index"},
		{installsFileName, "installs.json", "the install receipts"},
		{pinsFileName, "skill-pins.json", "the pin set"},
		{storeDirName, "store", "the content-addressed blob directory"},
		{backupsDirName, "backups", "the rolling backups directory"},
		{lockFileName, ".lock", "the cross-process lock"},
		{MarkerFileName, ".agenthub-managed.json", "the ownership marker Apply and Remove require"},
	} {
		if tc.got != tc.want {
			t.Errorf("frozen on-disk name changed: got %q, want %q (%s).\n"+
				"Renaming it orphans what is already on disk; if the rename is intended, "+
				"it needs a migration, not an edit to this line.", tc.got, tc.want, tc.why)
		}
	}
}
