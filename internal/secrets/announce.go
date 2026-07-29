package secrets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// This file is the credential announcement plane: the "something about this
// server's stored credentials changed" signal that the registry has and the
// vault did not.
//
// Why it has to exist at all. A credential is not part of a server's
// definition — the registry holds ${SECRET_X} placeholders and the OAuth
// bearer never appears in a spec — so a vault write moves nothing the
// registry watcher can see, and it must stay that way: putting credentials
// into the registry's comparison surface would mean the registry holds
// secrets. The result was that `agenthub auth login` on an already-enabled
// server wrote the token and told nobody, and every running client kept
// using the credential that had just been replaced until it was restarted.
//
// What is announced is deliberately NOT the credential: this file records
// server ids and a monotonic counter, nothing else. It is readable by
// anything that can read the directory, and that is fine precisely because
// there is no secret in it — a reader learns that "notion" got a new
// credential at some point, which is what it needs to act and all it gets.
//
// Backend independence is the reason it is a file of its own rather than
// watching the vault's storage. A credential may live in the OS keyring,
// where a value replaced in place changes no file at all — a watcher over
// <data>/secrets would see nothing and report nothing for the single most
// common case, a refreshed OAuth token.

// credentialsRevFile is the announcement file, a sibling of secrets.enc and
// the keyring key registry.
const credentialsRevFile = "credentials.rev"

// revDoc is the on-disk shape: server id → announcement counter.
//
// The counter is meaningful only by comparison — a reader keeps the last
// value it saw per server and acts when it differs. Absolute values, and
// gaps in them, mean nothing.
type revDoc struct {
	Servers map[string]uint64 `json:"servers"`
}

// Announce records that the stored credentials of serverID changed.
//
// It is last-write-wins by design and takes no lock: two writers racing may
// land the same counter value, and the cost of that is one missed
// announcement, recovered by the next one or by the reader's own fallback.
// A lock here would be a cross-process lock taken on the hot path of every
// token refresh, to protect a hint.
func Announce(dir, serverID string) error {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(serverID) == "" {
		return nil
	}
	path := filepath.Join(dir, credentialsRevFile)
	doc, err := loadRevDoc(path)
	if err != nil {
		return err
	}
	if doc.Servers == nil {
		doc.Servers = map[string]uint64{}
	}
	doc.Servers[serverID]++
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return atomicWrite0600(path, append(out, '\n'))
}

// Revisions reads the announcement counters. A missing file is an empty map
// and no error: nothing has ever been announced.
//
// An unreadable or malformed file is also reported as empty, WITHOUT an
// error, and this is the failure direction of the whole plane: an
// announcement that does not arrive costs promptness, never correctness.
// Every consumer has a fallback that predates this file — the re-dial ladder
// and the 401 retry both still run — so degrading to "no announcements" is
// degrading to exactly the behaviour of the release before it.
func Revisions(dir string) map[string]uint64 {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	doc, err := loadRevDoc(filepath.Join(dir, credentialsRevFile))
	if err != nil || doc.Servers == nil {
		return nil
	}
	return doc.Servers
}

func loadRevDoc(path string) (revDoc, error) {
	var doc revDoc
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		// A torn or hand-edited file must not wedge the writer: start a fresh
		// document. The counters are hints, so losing their history costs one
		// comparison, and the alternative — refusing every future
		// announcement — costs every future recovery.
		return revDoc{}, nil
	}
	return doc, nil
}

// announce publishes ref's server without letting a failure reach the
// caller: the credential IS stored by the time this runs, and reporting an
// announcement failure as a storage failure would be a lie in the direction
// that makes a user re-run a login that already worked.
func (c *Chain) announce(serverID string) {
	dir, err := c.baseDir()
	if err != nil {
		return
	}
	_ = Announce(dir, serverID)
}
