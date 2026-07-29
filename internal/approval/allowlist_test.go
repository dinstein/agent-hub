package approval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testEntry() Entry {
	return Entry{
		Fingerprint: "v1:aaaa",
		Server:      "github",
		Tool:        "delete_repo",
		GateReason:  ReasonDestructive,
		CreatedAt:   time.Now().UTC(),
	}
}

func TestOpenAllowlistMissingIsEmpty(t *testing.T) {
	al, err := OpenAllowlist(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if al.Match(testRequest()) {
		t.Fatal("empty allowlist matched")
	}
	if got := al.Entries(); len(got) != 0 {
		t.Fatalf("Entries = %d, want 0", len(got))
	}
}

func TestAllowlistAddMatchReopen(t *testing.T) {
	dir := t.TempDir()
	al, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := al.Add(testEntry()); err != nil {
		t.Fatal(err)
	}
	if !al.Match(testRequest()) {
		t.Fatal("added entry did not match")
	}

	path := filepath.Join(dir, AllowlistFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("allowlist perms = %v, want 0600", fi.Mode().Perm())
	}
	// Argument bytes must never appear on disk (memory-only invariant).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "a/b") {
		t.Fatal("argument bytes leaked into the allowlist file")
	}

	al2, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !al2.Match(testRequest()) {
		t.Fatal("entry lost across reopen")
	}
	drifted := testRequest()
	drifted.Fingerprint = "v1:bbbb"
	if al2.Match(drifted) {
		t.Fatal("drifted fingerprint matched (stale grants must miss)")
	}
}

func TestAllowlistBindings(t *testing.T) {
	e := testEntry()

	server := testRequest()
	server.Server = "other"
	if e.matches(server) {
		t.Fatal("server-bound entry matched a different server")
	}
	tool := testRequest()
	tool.Tool = "other_tool"
	if e.matches(tool) {
		t.Fatal("tool-bound entry matched a different tool")
	}

	// Unbound args: any hash matches.
	anyArgs := testRequest()
	anyArgs.ArgsHash = "hash-9"
	if !e.matches(anyArgs) {
		t.Fatal("args-unbound entry rejected a different args hash")
	}
	// Bound args: only the exact hash.
	e.ArgsHash = "hash-1"
	if !e.matches(testRequest()) {
		t.Fatal("args-bound entry rejected the exact hash")
	}
	if e.matches(anyArgs) {
		t.Fatal("args-bound entry matched a different hash")
	}

	// Unbound server/tool: fingerprint alone decides.
	loose := Entry{Fingerprint: "v1:aaaa"}
	if !loose.matches(server) || !loose.matches(tool) {
		t.Fatal("fingerprint-only entry should match any server/tool")
	}
}

func TestAllowlistEmptyFingerprintFailsClosed(t *testing.T) {
	al, err := OpenAllowlist(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := al.Add(Entry{Server: "github"}); err == nil {
		t.Fatal("Add without fingerprint succeeded")
	}
	if err := al.Add(testEntry()); err != nil {
		t.Fatal(err)
	}
	req := testRequest()
	req.Fingerprint = ""
	if al.Match(req) {
		t.Fatal("request without fingerprint matched (must always go to a human)")
	}
	e := Entry{}
	if e.matches(Request{}) {
		t.Fatal("empty-vs-empty fingerprint matched")
	}
}

func TestAllowlistCorruptFileFailsClosedAndIsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AllowlistFileName)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"garbage", "{not json"},
		{"empty", "   \n"},
		{"trailing", `{"version":1,"entries":{}} extra`},
		{"future-version", `{"version":99,"entries":{}}`},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenAllowlist(dir); err == nil {
			t.Fatalf("%s: OpenAllowlist succeeded on bad file", tc.name)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != tc.body {
			t.Fatalf("%s: bad file was modified (evidence must be preserved)", tc.name)
		}
	}
}

func TestAllowlistRemove(t *testing.T) {
	dir := t.TempDir()
	al, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := al.Add(testEntry()); err != nil {
		t.Fatal(err)
	}
	ok, err := al.Remove("v1:aaaa")
	if err != nil || !ok {
		t.Fatalf("Remove = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = al.Remove("v1:aaaa")
	if err != nil || ok {
		t.Fatalf("second Remove = (%v, %v), want (false, nil)", ok, err)
	}
	al2, err := OpenAllowlist(dir)
	if err != nil {
		t.Fatal(err)
	}
	if al2.Match(testRequest()) {
		t.Fatal("removed entry survived reopen")
	}
}

func TestNilAllowlistNeverMatches(t *testing.T) {
	var al *Allowlist
	if al.Match(testRequest()) {
		t.Fatal("nil allowlist matched")
	}
}
