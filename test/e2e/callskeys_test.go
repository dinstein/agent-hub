package e2e_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ledger's two tiers are separated by a key, and everything in this file
// is about what that key does to a HISTORY rather than to a write. Metadata is
// always recorded; signing and payload capture start at `calls enable`; and
// `calls rotate-key` leaves both behind it — records signed under a key that
// is no longer current, and packs sealed with it.
//
// So the file an auditor eventually reads is mixed by construction on any
// installation that ran for a while, and the reading has to stay correct
// across every seam in it. That is a statement about two processes and a file,
// which is why it is here rather than in internal/calllog.

// callsJSONL returns the path of the ledger's shared event file for whichever
// UTC day partition holds it. The layout is calls/<YYYY-MM-DD>/calls.jsonl and
// a test that hardcoded today's date would fail once a year, at midnight UTC,
// on somebody else's machine.
func callsJSONL(t *testing.T, dataDir string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(dataDir, "calls"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "calls.jsonl" {
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no calls.jsonl under %s/calls (walk err %v)", dataDir, err)
	}
	return found
}

// TestCallsVerifyReportsAnUnkeyedHistoryAsUnauthenticated is the default
// installation's own case, and it is the one that went wrong.
//
// Metadata recording is always on and `calls.enabled` is off by default, so a
// stock hub produces a ledger in which NOTHING is signed — by design, and
// documented as such: an unkeyed record leaves `mac` and `keyId` empty rather
// than filling them with something unverifiable, so that verification can
// report "unauthenticated", which is a different answer from "authentication
// failed" and must never be confused with it.
//
// Both verifiers used to confuse them. An empty key id failed key lookup and
// was counted as a verification failure, so `calls verify` on a stock
// installation printed FAILED with one failure per record and exited non-zero
// — telling an operator their audit trail had been tampered with, on a hub
// where nothing had gone wrong at all.
func TestCallsVerifyReportsAnUnkeyedHistoryAsUnauthenticated(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")

	// No `calls enable`: this is the default, and the metadata tier records
	// anyway.
	seen := map[string]bool{}
	recordOneCall(t, dataDir, "unkeyed-history", seen)

	code, out := runAgenthubExit(t, dataDir, "", "calls", "verify", "--json")
	if code != 0 {
		t.Fatalf("verify exited %d on an untouched default installation: %s", code, out)
	}
	var v callsVerifyResult
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &v); err != nil {
		t.Fatalf("calls verify data: %v\n%s", err, out)
	}
	if !v.OK || v.Failures != 0 {
		t.Fatalf("an unsigned history was reported as a verification failure: %+v", v)
	}
	if v.Events == 0 {
		t.Fatalf("the metadata tier recorded nothing at all: %+v", v)
	}
	// Every one of them, and no payloads: an unkeyed store has nothing to
	// seal a body with, so a non-zero count here would mean packs were written
	// that nothing can open.
	if v.Unauthenticated != v.Events {
		t.Fatalf("%d of %d events were unauthenticated; with no key it must be all of them: %+v",
			v.Unauthenticated, v.Events, v)
	}
	if v.Payloads != 0 {
		t.Fatalf("an unkeyed ledger stored %d payload(s): %+v", v.Payloads, v)
	}

	// The count must reach a human. "ok" over a history nothing signed is
	// true and, on its own, misleading.
	human, _ := runAgenthub(t, dataDir, "", "calls", "verify")
	if !strings.Contains(human, "unauthenticated") {
		t.Fatalf("the human report does not say the history is unauthenticated:\n%s", human)
	}

	// Turning the ledger on from here must not retrospectively change what the
	// earlier records are: they stay unauthenticated, the new ones are signed,
	// and both are in one report.
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")
	unkeyed := v.Unauthenticated
	recordOneCall(t, dataDir, "keyed-history", seen)

	v = callsVerify(t, dataDir)
	if !v.OK || v.Failures != 0 {
		t.Fatalf("a mixed history failed verification: %+v", v)
	}
	if v.Unauthenticated != unkeyed {
		t.Fatalf("the unauthenticated count moved from %d to %d; the old records were rewritten",
			unkeyed, v.Unauthenticated)
	}
	if v.Payloads == 0 || v.Events <= unkeyed {
		t.Fatalf("nothing was signed or sealed after `calls enable`: %+v", v)
	}
}

// TestCallsVerifyRefusesAStrippedKeyID is the boundary of the classification
// above, and the reason `Unauthenticated` requires BOTH fields empty.
//
// If "no key id" alone meant unauthenticated, blanking that one field on a
// signed record would downgrade a tampered event to a benign one — and the
// verifier would report `ok`. The writer sets `keyId` and `mac` together, so
// one without the other cannot come from agenthub: it is corruption, or an
// edit, and either way it belongs on the failure side.
func TestCallsVerifyRefusesAStrippedKeyID(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")
	recordOneCall(t, dataDir, "tamper-me", map[string]bool{})

	if v := callsVerify(t, dataDir); !v.OK {
		t.Fatalf("the ledger was already failing before it was edited: %+v", v)
	}

	// Strip the key id from one signed event, leaving its MAC in place.
	path := callsJSONL(t, dataDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	edited := false
	for i, line := range lines {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["keyId"] == nil || e["keyId"] == "" || e["mac"] == nil || e["mac"] == "" {
			continue
		}
		e["keyId"] = ""
		next, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("re-encoding event: %v", err)
		}
		lines[i] = string(next)
		edited = true
		break
	}
	if !edited {
		t.Fatalf("no signed event to edit in %s; the fixture recorded nothing keyed", path)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	code, out := runAgenthubExit(t, dataDir, "", "calls", "verify", "--json")
	if code == 0 {
		t.Fatalf("a stripped key id verified clean: %s", out)
	}
	var v callsVerifyResult
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &v); err != nil {
		t.Fatalf("calls verify data: %v\n%s", err, out)
	}
	if v.OK || v.Failures == 0 {
		t.Fatalf("the edited event was not reported as a failure: %+v", v)
	}
	if v.Unauthenticated != 0 {
		t.Fatalf("the edit was reclassified as merely unauthenticated: %+v", v)
	}
}

// TestCallsRotationKeepsRetainedHistoryReadable pins the rotation invariant:
// old key refs are retained while retained data still needs them.
//
// Rotation is the operation an installation performs precisely because it no
// longer trusts the previous key, and the temptation is to stop there. But the
// packs already on disk are sealed with it, so dropping the ref would destroy
// exactly the history somebody rotated in order to keep trustworthy — silently,
// and only discovered by the auditor who came looking later.
func TestCallsRotationKeepsRetainedHistoryReadable(t *testing.T) {
	dataDir := t.TempDir()
	const (
		before = "sealed-under-the-old-key"
		after  = "sealed-under-the-new-key"
	)
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")

	seen := map[string]bool{}
	oldCall := recordOneCall(t, dataDir, before, seen)

	out, _ := runAgenthub(t, dataDir, "", "calls", "rotate-key", "--json")
	var rot struct {
		PreviousKeyID string `json:"previousKeyId"`
		KeyID         string `json:"keyId"`
		Enabled       bool   `json:"enabled"`
	}
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &rot); err != nil {
		t.Fatalf("calls rotate-key data: %v\n%s", err, out)
	}
	if rot.KeyID == "" || rot.PreviousKeyID == "" || rot.KeyID == rot.PreviousKeyID {
		t.Fatalf("rotation did not produce a new key: %+v", rot)
	}
	if !rot.Enabled {
		t.Fatal("rotation switched recording off")
	}

	newCall := recordOneCall(t, dataDir, after, seen)
	if newCall == oldCall {
		t.Fatal("the second call was not recorded separately")
	}

	// Both bodies, across the seam, from a process that holds neither key
	// until it looks the right one up by id.
	for _, tc := range []struct{ id, marker string }{{oldCall, before}, {newCall, after}} {
		out, _ = runAgenthub(t, dataDir, "", "calls", "show", tc.id, "--payloads", "--json")
		if !strings.Contains(out, tc.marker) {
			t.Fatalf("call %s is no longer readable after the rotation: %s", tc.id, out)
		}
	}

	// And the whole mixed history authenticates in one pass: verify resolves
	// each record's key by the id the record carries, so a rotation that had
	// orphaned the old ref shows up here as failures rather than as a gap
	// nobody notices.
	v := callsVerify(t, dataDir)
	if !v.OK || v.Failures != 0 {
		t.Fatalf("the history spanning a rotation failed verification: %+v", v)
	}
	if v.Payloads < 2 {
		t.Fatalf("verify opened %d payload(s) across two recorded calls: %+v", v.Payloads, v)
	}
}
