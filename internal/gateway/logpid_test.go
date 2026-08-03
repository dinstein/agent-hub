package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// TestGatewayLogCarriesThePID is the regression test for a log file that
// could not say who wrote it. Every `agenthub connect --client <id>` of one
// client appends to gateway-<id>.log, and a user normally has several
// running, so without the pid two gateways' lines read as one gateway doing
// impossible things.
func TestGatewayLogCarriesThePID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	g, c, _ := startGateway(t, Config{ClientID: "pidtest", Resolver: resolver, Log: log})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	g.log.Info("a record that must be attributable")

	want := os.Getpid()
	var checked int
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, line)
		}
		pid, ok := rec[logx.FieldPID]
		if !ok {
			t.Fatalf("log record has no %q field: %s", logx.FieldPID, line)
		}
		if int(pid.(float64)) != want {
			t.Fatalf("record reports pid %v, want %d", pid, want)
		}
		if _, ok := rec[logx.FieldClient]; !ok {
			t.Fatalf("the pid must ADD to the client field, not replace it: %s", line)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no log records were produced, so nothing was verified")
	}
}

// TestGatewayLogNeverRepeatsTheClientKey is the regression test for a
// mandatory field that two lines quietly redefined.
//
// logx.FieldClient is bound once, at construction, and holds the CONFIGURED
// client id — the value every stream is joined on. The handshake lines then
// passed the PEER's self-reported name under the same key. slog's JSON
// handler does not deduplicate, so the field was emitted twice on one line,
// and a reader taking the last of two (most do) read "Claude Code" where the
// join key is "claude-code".
//
// The check is on the serialized line rather than on a decoded map, because
// encoding/json silently keeps only the last of two duplicate keys — a
// decoded map cannot see the bug at all.
func TestGatewayLogNeverRepeatsTheClientKey(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	_, c, _ := startGateway(t, Config{ClientID: "dup-test", Resolver: resolver, Log: log})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	key := `"` + logx.FieldClient + `":`
	var checked int
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if n := strings.Count(line, key); n > 1 {
			t.Fatalf("the %q field appears %d times on one line: %s", logx.FieldClient, n, line)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no log records were produced, so nothing was verified")
	}
}

// LogPath is the one place the gateway log's file name is composed, and
// `agenthub logs` reads it back through the same function. Two properties
// matter to that reader and are pinned here.
func TestLogPathNamingContract(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("x", "logs")

	if got, want := LogPath(dir, "claude-code"), filepath.Join(dir, "gateway-claude-code.log"); got != want {
		t.Fatalf("LogPath = %q, want %q", got, want)
	}

	// Every name a reader enumerates is bracketed by the exported pair, so
	// building the glob from them cannot miss a file the writer produced.
	got := LogPath(dir, "a.b/c d")
	base := filepath.Base(got)
	if !strings.HasPrefix(base, LogFilePrefix) || !strings.HasSuffix(base, LogFileExt) {
		t.Fatalf("%q is not bracketed by %q/%q", base, LogFilePrefix, LogFileExt)
	}

	// The mapping is MANY-TO-ONE. A reader narrowing by client may use the
	// path to pick files (it never under-selects) but must then filter on
	// the record's own client field, which keeps the unsanitized id.
	if LogPath(dir, "a.b") != LogPath(dir, "a/b") {
		t.Fatal("fsSafe no longer collapses distinct ids; the reader's exact-match rule assumed it does")
	}
}
