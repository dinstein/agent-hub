package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The call ledger is the one subsystem here whose entire purpose is to be read
// LATER, by somebody who was not there — a different process, after a restart,
// possibly after a key rotation. Every claim it makes is therefore a claim
// about two processes agreeing about a file, and `calls enable` + `calls show`
// were the only two of its ten verbs this suite drove.
//
// What the rest are for splits cleanly in two, and the split is the reason to
// test them together: `tail`, `stats` and a plain `show` must answer WITHOUT
// decrypting anything, so that reading the ledger to find out what happened is
// not itself a disclosure; `show --payloads`, `export --payloads` and `verify`
// are the ones that open the packs. A test that only checked the second half
// would pass just as happily against a ledger that stored the bodies in the
// clear.

// callRow is the slice of cli.CallEventRow these tests read back.
type callRow struct {
	CallID  string `json:"callId"`
	Event   string `json:"event"`
	Client  string `json:"client"`
	Method  string `json:"method"`
	Surface string `json:"surface"`
	Server  string `json:"server"`
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
}

// callsTail reads the whole ledger back as rows, plus the raw output — the
// raw text is what the disclosure assertions are made against, because a
// secret leaking through a field this struct does not model would be invisible
// to a decoded comparison.
func callsTail(t *testing.T, dataDir string) ([]callRow, string) {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "calls", "tail", "--since", "all", "--limit", "0", "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("calls tail: %s", out)
	}
	var tail struct {
		Events []callRow `json:"events"`
	}
	if err := json.Unmarshal(env.Data, &tail); err != nil {
		t.Fatalf("calls tail data: %v\n%s", err, env.Data)
	}
	return tail.Events, out
}

// waitFinishedCall polls until a finished tools/call for tool shows up whose
// id is not already in seen, and returns it. The finish is written after the
// downstream has answered, and the ledger writer is not on the call's critical
// path, so a read taken the instant callTool returns can legitimately miss it.
//
// Excluding what was already there is what makes a second recording safe: a
// test that records twice — either side of a key rotation — would otherwise be
// handed the earlier call the moment it asked, and would then make every
// assertion about the wrong one.
func waitFinishedCall(t *testing.T, dataDir, tool string, seen map[string]bool, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		rows, _ := callsTail(t, dataDir)
		for _, r := range rows {
			if r.Event == "finished" && r.Method == "tools/call" && r.Tool == tool && !seen[r.CallID] {
				seen[r.CallID] = true
				return r.CallID
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no new finished tools/call for %q in the ledger within %s: %+v", tool, budget, rows)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// recordOneCall is the shared stage: a live gateway and one call whose
// arguments carry marker. seen accumulates the call ids already accounted for,
// so repeated use in one test always returns the newest.
func recordOneCall(t *testing.T, dataDir, marker string, seen map[string]bool) string {
	t.Helper()
	c := startGateway(t, dataDir, "ledgerclient")
	c.initialize()
	c.waitForTool("alpha__echo", 45*time.Second)
	if got := c.textContent(c.callTool("alpha__echo", map[string]any{"marker": marker}, 45*time.Second)); !strings.Contains(got, marker) {
		t.Fatalf("the fixture call did not echo its marker: %q", got)
	}
	c.close()
	return waitFinishedCall(t, dataDir, "echo", seen, 30*time.Second)
}

// TestCallsLedgerReadsWithoutDecryptingUntilAsked walks the ledger's read
// surface over one real recorded call.
//
// The marker is the instrument. It travels in the call's arguments and comes
// back in its result, so it is in both captured bodies and in neither piece of
// metadata — which makes "did this command decrypt anything" a question about
// one substring rather than about which fields a reader happens to model.
func TestCallsLedgerReadsWithoutDecryptingUntilAsked(t *testing.T) {
	dataDir := t.TempDir()
	const marker = "ledger-secret-payload"

	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")

	callID := recordOneCall(t, dataDir, marker, map[string]bool{})

	// `status` is the operator's answer to "is anything being recorded, and
	// under which key". Without the key id a rotation cannot be observed at
	// all, which is what the next test needs.
	out, _ := runAgenthub(t, dataDir, "", "calls", "status", "--json")
	var status struct {
		Enabled bool   `json:"enabled"`
		KeyID   string `json:"keyId"`
	}
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &status); err != nil {
		t.Fatalf("calls status data: %v\n%s", err, out)
	}
	if !status.Enabled || status.KeyID == "" {
		t.Fatalf("the ledger reports itself off or unkeyed after `calls enable`: %+v", status)
	}

	// tail: metadata, and the metadata is real — a row that named no server
	// would satisfy a substring check just as well as a correct one.
	rows, rawTail := callsTail(t, dataDir)
	var found bool
	for _, r := range rows {
		if r.CallID == callID && r.Event == "finished" {
			found = true
			if r.Server != "alpha" || r.Tool != "echo" || r.Outcome == "" || r.Client != "ledgerclient" {
				t.Fatalf("the finished row does not describe the call that happened: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("call %s is not in its own tail: %+v", callID, rows)
	}
	if strings.Contains(rawTail, marker) {
		t.Fatal("`calls tail` decrypted a payload; reading the ledger must not be a disclosure")
	}

	// stats: aggregates over the same events, still without opening a pack.
	out, _ = runAgenthub(t, dataDir, "", "calls", "stats", "--since", "all", "--json")
	var stats struct {
		Calls   int            `json:"calls"`
		Servers map[string]int `json:"servers"`
	}
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &stats); err != nil {
		t.Fatalf("calls stats data: %v\n%s", err, out)
	}
	if stats.Calls == 0 || stats.Servers["alpha"] == 0 {
		t.Fatalf("stats saw no call to alpha: %+v", stats)
	}
	if strings.Contains(out, marker) {
		t.Fatal("`calls stats` decrypted a payload")
	}

	// show: the same call, both ways round. This pair is the whole contract —
	// the bodies exist and are reachable, and reaching them takes asking.
	out, _ = runAgenthub(t, dataDir, "", "calls", "show", callID, "--json")
	if strings.Contains(out, marker) {
		t.Fatal("`calls show` without --payloads returned a decrypted body")
	}
	out, _ = runAgenthub(t, dataDir, "", "calls", "show", callID, "--payloads", "--json")
	if !strings.Contains(out, marker) {
		t.Fatalf("`calls show --payloads` did not return the captured body: %s", out)
	}

	// verify: authenticate every event and decrypt every payload it names.
	// This is the command an auditor runs, and the only one that touches all
	// of both tiers.
	v := callsVerify(t, dataDir)
	if !v.OK || v.Failures != 0 {
		t.Fatalf("verify failed on a ledger nothing had touched: %+v", v)
	}
	if v.Events == 0 || v.Payloads == 0 {
		t.Fatalf("verify authenticated nothing: %+v", v)
	}
}

// callsVerifyResult is the slice of cli.CallsVerify these tests read.
type callsVerifyResult struct {
	OK              bool     `json:"ok"`
	Events          int      `json:"events"`
	Payloads        int      `json:"payloads"`
	Unauthenticated int      `json:"unauthenticated"`
	Failures        int      `json:"failures"`
	Issues          []string `json:"issues"`
}

func callsVerify(t *testing.T, dataDir string) callsVerifyResult {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "calls", "verify", "--json")
	var v callsVerifyResult
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &v); err != nil {
		t.Fatalf("calls verify data: %v\n%s", err, out)
	}
	return v
}

// TestCallsExportIsPrivateAndRefusesToOverwrite covers the one verb that takes
// the ledger OUT of agenthub's directory.
//
// Two properties matter more here than anywhere else in the subsystem. The
// destination must be created private, because an export with payloads is a
// plaintext copy of everything the packs protect and it lands wherever the
// operator was standing. And it must refuse an existing path rather than
// truncate it: the argument is a filename typed by hand, under time pressure,
// usually during an incident.
func TestCallsExportIsPrivateAndRefusesToOverwrite(t *testing.T) {
	dataDir := t.TempDir()
	const marker = "exported-secret-payload"

	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")
	recordOneCall(t, dataDir, marker, map[string]bool{})

	outDir := t.TempDir()
	plain := filepath.Join(outDir, "metadata.jsonl")
	out, _ := runAgenthub(t, dataDir, "", "calls", "export", "--output", plain, "--since", "all", "--json")
	var exported struct {
		Events   int  `json:"events"`
		Payloads bool `json:"payloads"`
	}
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &exported); err != nil {
		t.Fatalf("calls export data: %v\n%s", err, out)
	}
	if exported.Events == 0 || exported.Payloads {
		t.Fatalf("a plain export reported %+v", exported)
	}
	info, err := os.Stat(plain)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the export is readable by others: mode %o", perm)
	}
	body, err := os.ReadFile(plain)
	if err != nil {
		t.Fatalf("reading export: %v", err)
	}
	if strings.Contains(string(body), marker) {
		t.Fatal("an export without --payloads carried a decrypted body")
	}

	// The same path again must be refused. Truncating it would destroy the
	// export somebody just took, which is the file least likely to have been
	// copied anywhere yet.
	code, refused := runAgenthubExit(t, dataDir, "", "calls", "export", "--output", plain, "--json")
	if code == 0 {
		t.Fatalf("export overwrote an existing file: %s", refused)
	}
	if again, err := os.ReadFile(plain); err != nil || string(again) != string(body) {
		t.Fatalf("the refused export still changed the file (err %v)", err)
	}

	// And with payloads the bodies really are in there: the flag has to be
	// the difference, not the wording of a warning.
	withBodies := filepath.Join(outDir, "full.jsonl")
	runAgenthub(t, dataDir, "", "calls", "export", "--output", withBodies, "--since", "all", "--payloads", "--json")
	full, err := os.ReadFile(withBodies)
	if err != nil {
		t.Fatalf("reading full export: %v", err)
	}
	if !strings.Contains(string(full), marker) {
		t.Fatal("`calls export --payloads` produced no decrypted body")
	}
	if info, err = os.Stat(withBodies); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the payload export is not private: %v %o", err, info.Mode().Perm())
	}
}
