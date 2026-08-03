package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

func openTest(t *testing.T) (*Stream, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, Options{PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestAppendStampsWhatCallersMustNotHaveToRemember(t *testing.T) {
	s, path := openTest(t)
	before := time.Now().UTC()
	s.Append(Record{Scope: ScopeServer, Kind: KindConnected, Server: "github"})
	s.Sync()

	res, err := Read(path, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records = %+v", res.Records)
	}
	got := res.Records[0]
	// The pid is stamped by the stream, not the caller: a call site that can
	// forget it is a call site that eventually does, and with N gateways
	// sharing this file a record with no pid cannot be attributed at all.
	if got.PID != 4242 {
		t.Errorf("pid = %d, want the stream's", got.PID)
	}
	if got.TS.Before(before) || got.TS.Location() != time.UTC {
		t.Errorf("ts = %v, want a UTC instant at or after %v", got.TS, before)
	}
	if got.Server != "github" || got.Kind != KindConnected {
		t.Errorf("record = %+v", got)
	}
}

// The nil Stream is what makes "the switch is off" and "the file would not
// open" one code path at every call site.
func TestNilStreamIsUsable(t *testing.T) {
	var s *Stream
	s.Append(Record{Scope: ScopeDaemon, Kind: KindDaemonStarted})
	s.Sync()
	if got := s.Dropped(); got != 0 {
		t.Errorf("Dropped = %d", got)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// A record must never become an oversize marker: the marker unmarshals into
// a Record with no kind, which reads as a blank row claiming nothing
// happened. Detail is fitted to the SERIALIZED size for exactly this.
func TestLongDetailStillProducesAReadableRecord(t *testing.T) {
	s, path := openTest(t)
	// Quote-heavy, so escaping roughly doubles the serialized length and a
	// raw-byte cap alone would not be enough.
	s.Append(Record{
		Scope: ScopeServer, Kind: KindConnectFailed, Server: "github",
		Detail: strings.Repeat(`he said "no" \ `, 2000),
	})
	s.Sync()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if len(line)+1 > jsonl.DefaultMaxLineBytes {
			t.Fatalf("line is %d bytes, over the %d budget that keeps appends atomic",
				len(line)+1, jsonl.DefaultMaxLineBytes)
		}
		if _, ok := jsonl.DecodeOversize([]byte(line)); ok {
			t.Fatal("the record was replaced by an oversize marker")
		}
	}
	res, err := Read(path, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 || res.Skipped != 0 {
		t.Fatalf("read back %d records, %d skipped", len(res.Records), res.Skipped)
	}
	if res.Records[0].Kind != KindConnectFailed || res.Records[0].Detail == "" {
		t.Fatalf("record lost its identity: %+v", res.Records[0])
	}
}

// The retired savings projection read only the active file and silently
// under-reported everything rotation had moved aside. This is the
// regression test for not repeating it.
func TestReadCoversRotatedSegments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)

	write := func(p string, msgs ...string) {
		var b strings.Builder
		for _, m := range msgs {
			line, err := json.Marshal(Record{
				TS: time.Unix(0, 0).UTC(), Scope: ScopeServer,
				Kind: KindConnected, Server: m, PID: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two rotated segments plus the active file. The stamps are what
	// jsonl.segmentPath produces, and they sort chronologically by name.
	write(base+"-20200102T030405.000000000Z.p11"+ext, "oldest")
	write(base+"-20200103T030405.000000000Z.p12"+ext, "middle")
	write(path, "newest")

	res, err := Read(path, Query{})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, r := range res.Records {
		seen = append(seen, r.Server)
	}
	if !slices.Equal(seen, []string{"oldest", "middle", "newest"}) {
		t.Fatalf("read = %v, want every segment oldest first", seen)
	}
	if len(res.Files) != 3 {
		t.Fatalf("Files = %v, want all three so a report can say what it covers", res.Files)
	}
}

// Retention has to be enforced somewhere, and a stream that is on by default
// must not grow without bound.
func TestOpenPrunesOldSegments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)

	var made []string
	for i := range keepSegments + 2 {
		p := base + "-2020010" + string(rune('1'+i)) + "T030405.000000000Z.p1" + ext
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		made = append(made, p)
	}

	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	for i, p := range made {
		wantGone := i < len(made)-keepSegments
		_, err := os.Stat(p)
		if wantGone && err == nil {
			t.Errorf("%s survived the prune", filepath.Base(p))
		}
		if !wantGone && err != nil {
			t.Errorf("%s was pruned; the newest %d must survive", filepath.Base(p), keepSegments)
		}
	}
	// Never the active file, whatever the count says.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the active file was pruned: %v", err)
	}
}

// A torn tail from a killed writer must not make the whole stream
// unreadable, and must not pass unmentioned either.
func TestUndecodableLinesAreCountedNotFatal(t *testing.T) {
	s, path := openTest(t)
	s.Append(Record{Scope: ScopeServer, Kind: KindConnected, Server: "github"})
	s.Sync()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2020-01-01T00:00:0`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	res, err := Read(path, Query{})
	if err != nil {
		t.Fatalf("a torn line made the read fail: %v", err)
	}
	if len(res.Records) != 1 {
		t.Errorf("the torn line cost the good record: %+v", res.Records)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 — an unreadable line must be reported", res.Skipped)
	}
}

func TestQueryNarrowsOnEveryAxis(t *testing.T) {
	s, path := openTest(t)
	s.Append(Record{TS: time.Unix(100, 0), Scope: ScopeServer, Kind: KindConnected, Server: "github", Client: "a"})
	s.Append(Record{TS: time.Unix(200, 0), Scope: ScopeServer, Kind: KindCircuitOpen, Server: "linear", Client: "b"})
	s.Append(Record{TS: time.Unix(300, 0), Scope: ScopeDaemon, Kind: KindDaemonStarted})
	s.Sync()

	count := func(q Query) int {
		res, err := Read(path, q)
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Records)
	}
	if got := count(Query{}); got != 3 {
		t.Fatalf("the zero Query is a rule, not a filter: got %d", got)
	}
	if got := count(Query{Scope: ScopeServer}); got != 2 {
		t.Errorf("Scope = %d", got)
	}
	if got := count(Query{Server: "linear"}); got != 1 {
		t.Errorf("Server = %d", got)
	}
	if got := count(Query{Client: "a"}); got != 1 {
		t.Errorf("Client = %d", got)
	}
	if got := count(Query{Kinds: []Kind{KindCircuitOpen, KindDaemonStarted}}); got != 2 {
		t.Errorf("Kinds = %d", got)
	}
	if got := count(Query{Since: time.Unix(200, 0).UTC()}); got != 2 {
		t.Errorf("Since = %d", got)
	}
}

// The vocabulary is closed, and a constant outside allKinds is a constant no
// consumer can be told about.
func TestEveryKindIsInTheClosedSet(t *testing.T) {
	if len(KindNames("")) == 0 {
		t.Fatal("the closed set is empty, so this test asserted nothing")
	}
	// Two scopes legitimately share a spelling (a gateway and the daemon
	// both "start"), so a duplicate is only a fault WITHIN one scope.
	for scope, kinds := range allKinds {
		seen := map[Kind]bool{}
		for _, k := range kinds {
			if seen[k] {
				t.Errorf("scope %q lists %q twice", scope, k)
			}
			seen[k] = true
		}
	}
	if !KnownKind(ScopeServer, KindConnected) {
		t.Error("KnownKind rejects a pair it defines")
	}
	// The pair is what is checked, never the kind alone: `started` is real
	// at gateway and daemon scope and meaningless at server scope.
	if KnownKind(ScopeServer, KindGatewayStarted) {
		t.Error("KnownKind accepted a kind from another scope")
	}
	if KnownKind("nonsense", KindConnected) {
		t.Error("KnownKind accepted an unknown scope")
	}
	// The empty scope is "at any scope", not "an unknown scope". Two callers
	// narrow by kind without naming one, and answering them `false` would
	// turn every such query into a usage error.
	if !KnownKind("", KindGatewayStarted) {
		t.Error("KnownKind rejected a real kind under the any-scope query")
	}
	if KnownKind("", "no_such_kind") {
		t.Error("KnownKind accepted an invented kind under the any-scope query")
	}
	if KnownScope("nonsense") || !KnownScope(ScopeDaemon) {
		t.Error("KnownScope disagrees with the closed set")
	}
}

// scopeOrder is what every hint, help string and listing is built from,
// while KnownScope answers from allKinds. A scope in one and not the other
// would be accepted by the validator and invisible in the list of what may
// be asked for — a selector no error message admits exists.
func TestScopeOrderCoversTheVocabulary(t *testing.T) {
	if len(scopeOrder) != len(allKinds) {
		t.Fatalf("scopeOrder has %d scopes, allKinds %d", len(scopeOrder), len(allKinds))
	}
	for _, s := range scopeOrder {
		if !KnownScope(s) {
			t.Errorf("scopeOrder lists %q, which allKinds does not define", s)
		}
	}
	// KindNames("") is the union, so it must contain every scope's kinds.
	union := KindNames("")
	for scope, kinds := range allKinds {
		for _, k := range kinds {
			if !slices.Contains(union, string(k)) {
				t.Errorf("kind %q at scope %q is missing from the any-scope listing", k, scope)
			}
		}
	}
}
