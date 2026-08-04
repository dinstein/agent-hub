package gateway

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// The scope chain decides what a client may reach, and until this line it
// decided it silently: the result was readable only through `agenthub
// session`, from outside and after the fact. A session that is narrow from
// its first moment is the ordinary case, so the startup shape is what makes
// "why can't my client see this tool" answerable from the log at all.
func TestStartupLogsTheEffectiveScopeShape(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "scopeshape", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"s1": fakemcp.Minimal("echo"),
			"s2": fakemcp.Minimal("echo"),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo", "s2__echo")

	start := findScope(t, sink, "startup")
	if start["level"] != slog.LevelInfo.String() {
		t.Errorf("the scope shape logged at %s, want INFO", start["level"])
	}

	// The startup line describes the COLD catalog — on a first run, nothing —
	// and the shape a client actually sees arrives with the catalog swap. The
	// test pins both so the sequence is the documented one rather than an
	// accident of timing.
	//
	// Waited for rather than read once, because tools/list going green is not
	// evidence that the matching log line exists yet: swapCatalog publishes
	// the catalog under the lock and logs the new shape after releasing it, so
	// waitForTools above can return in the window between the two. The wait is
	// on the settled server count for the same reason the lookup takes the
	// last record — the two downstreams dial independently, and one swap each
	// is as correct as one swap for both.
	var swapped map[string]string
	waitFor(t, "the scope shape for the settled catalog", func() bool {
		rec, ok := lookupScope(sink, "catalog changed")
		if !ok {
			return false
		}
		swapped = rec
		return rec["servers"] == "2"
	})
	// Counts, never names: the line must not grow with the catalog, which is
	// the size at which it would stop being readable.
	if swapped["tools"] != "2" {
		t.Errorf("tools = %q, want 2", swapped["tools"])
	}
}

// lookupScope returns the scope-shape record logged for one reason. The
// reason is what tells the cold startup line apart from the one describing a
// live catalog, and the two carry different counts on purpose.
//
// The LAST match, not the first, and that is the whole of a flake this test
// carried on Linux CI: "catalog changed" is logged once per catalog swap
// (downstreams.go), and downstreams connect independently, so two servers
// produce either one swap carrying both or two swaps carrying one each,
// depending on how the machine schedules the dials. Reading the first match
// asserted on whichever intermediate state happened to be recorded — servers
// = 1 on a loaded runner, 2 on a fast laptop — for a question that is about
// where the catalog SETTLED. Every caller waits for the settled state first,
// which is what makes the last record the deterministic one.
func lookupScope(sink *callLog, reason string) (map[string]string, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, r := range slices.Backward(sink.recs) {
		if r["msg"] == "effective scope resolved" && r["reason"] == reason {
			return r, true
		}
	}
	return nil, false
}

func findScope(t *testing.T, sink *callLog, reason string) map[string]string {
	t.Helper()
	rec, ok := lookupScope(sink, reason)
	if !ok {
		sink.mu.Lock()
		seen := sink.seen
		sink.mu.Unlock()
		t.Fatalf("no scope shape logged with reason %q; logged: %v", reason, seen)
	}
	return rec
}

// A dangling profile reference fails CLOSED to an empty scope, so its
// diagnostic describes a client that can suddenly see nothing. The scope
// value has carried these all along and the gateway read none of them —
// `agenthub session` was the only consumer — which left the loudest possible
// symptom with the quietest possible explanation.
func TestDanglingProfileDiagnosticReachesTheLog(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1")
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Clients.V.Clients["danglingprof"] = registry.Doc[registry.ClientEntry]{
			V: registry.ClientEntry{Profile: "nonexistent"},
		}
	})

	log, sink := newCallLog()
	_, c, _ := startGateway(t, Config{
		ClientID: "danglingprof", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{"s1": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	rec := sink.find(t, "scope diagnostic")
	if rec["level"] != slog.LevelWarn.String() {
		t.Errorf("a fail-closed diagnostic logged at %s, want WARN", rec["level"])
	}
	if rec["layer"] != "profile" {
		t.Errorf("layer = %q, want profile", rec["layer"])
	}
	if !strings.Contains(rec["detail"], "nonexistent") {
		t.Errorf("the diagnostic does not name the missing profile: %q", rec["detail"])
	}

	// The shape must agree with it once a live catalog exists: fail-closed
	// means zero servers even though s1 connected, because narrowing is a
	// query-time projection and never touches the connection plane. A
	// diagnostic printed beside a scope still reporting servers would be the
	// worse of the two readings.
	waitFor(t, "the scope shape for the live catalog", func() bool {
		_, ok := lookupScope(sink, "catalog changed")
		return ok
	})
	shape := findScope(t, sink, "catalog changed")
	if shape["servers"] != "0" {
		t.Errorf("servers = %q, want 0 — a dangling profile fails closed", shape["servers"])
	}
}
