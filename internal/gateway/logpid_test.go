package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
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
