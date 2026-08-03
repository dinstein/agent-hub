package gateway

import (
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// readEvents drains the control-plane stream from a gateway's data dir.
func readEvents(t *testing.T, resolver *platform.Resolver) []eventlog.Record {
	t.Helper()
	dir, err := resolver.LogsDir()
	if err != nil {
		t.Fatal(err)
	}
	res, err := eventlog.Read(filepath.Join(dir, eventlog.FileName), eventlog.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 {
		t.Fatalf("%d undecodable event lines", res.Skipped)
	}
	return res.Records
}

func kindsOf(records []eventlog.Record, scope eventlog.Scope) []eventlog.Kind {
	var out []eventlog.Kind
	for _, r := range records {
		if r.Scope == scope {
			out = append(out, r.Kind)
		}
	}
	return out
}

func hasKind(records []eventlog.Record, scope eventlog.Scope, kind eventlog.Kind) bool {
	for _, k := range kindsOf(records, scope) {
		if k == kind {
			return true
		}
	}
	return false
}

// The end-to-end claim: a gateway that connects a server records it, under
// the right scope, with the identity a reader needs to place the record.
func TestGatewayRecordsServerLifecycle(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	_, c, _ := startGateway(t, Config{
		ClientID: "events-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")

	var connected eventlog.Record
	waitFor(t, "a server connected event", func() bool {
		for _, r := range readEvents(t, resolver) {
			if r.Scope == eventlog.ScopeServer && r.Kind == eventlog.KindConnected {
				connected = r
				return true
			}
		}
		return false
	})

	if connected.Server != "fake" {
		t.Errorf("connected.Server = %q", connected.Server)
	}
	// Client and pid are what make a shared file readable: N gateways append
	// here, and a record that cannot say which process observed it cannot be
	// placed in a timeline at all.
	if connected.Client != "events-client" {
		t.Errorf("connected.Client = %q, want the gateway's client", connected.Client)
	}
	if connected.PID == 0 {
		t.Error("connected.PID is unset; a record written by no process is not a state that exists")
	}
	if !hasKind(readEvents(t, resolver), eventlog.ScopeGateway, eventlog.KindGatewayStarted) {
		t.Error("the gateway did not record its own start")
	}
}

// The switch is the whole reason a default-on stream is acceptable, so it
// has to actually stop the writing rather than merely hide the reading.
func TestEventsSwitchOffWritesNothing(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) {
		off := false
		g.Events = &off
	})

	_, c, _ := startGateway(t, Config{
		ClientID: "events-off",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	// A connect that produced records would have produced them by now: the
	// tool is listed, which means the handshake completed.
	if got := readEvents(t, resolver); len(got) != 0 {
		t.Fatalf("the switch is off and %d events were written: %+v", len(got), got)
	}
}

// Absent means ON. A fresh installation has no governance document at all,
// and that is precisely the installation whose first incident nobody has
// configured for yet.
func TestEventsDefaultOnWithNoGovernance(t *testing.T) {
	t.Parallel()
	var g registry.GovernanceDoc
	if !g.EventsEnabled() {
		t.Fatal("an untouched governance document must leave the stream on")
	}
}
