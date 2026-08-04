package gateway

import (
	"bytes"
	"log/slog"
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
	swapped := findScope(t, sink, "catalog changed")
	// Counts, never names: the line must not grow with the catalog, which is
	// the size at which it would stop being readable.
	if swapped["servers"] != "2" {
		t.Errorf("servers = %q, want 2", swapped["servers"])
	}
	if swapped["tools"] != "2" {
		t.Errorf("tools = %q, want 2", swapped["tools"])
	}
}

// The shape says what a session ended up with; only the convergence says
// which layer took the rest away. With three intersecting layers and no way
// for any of them to widen, a client seeing nothing has exactly one layer to
// blame, and finding it meant reading three config files and guessing.
func TestScopeConvergenceIsLoggedAtDebug(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1"}, // s2 is dropped HERE, and nowhere else
		}}
		tx.Clients.V.Clients["convergence"] = registry.Doc[registry.ClientEntry]{
			V: registry.ClientEntry{Profile: "team"},
		}
	})

	log, sink := newCallLog() // this handler is Enabled at every level
	_, c, _ := startGateway(t, Config{
		ClientID: "convergence", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{
			"s1": fakemcp.Minimal("echo"),
			"s2": fakemcp.Minimal("echo"),
		}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo")

	rec := sink.find(t, "scope layer applied")
	if rec["level"] != slog.LevelDebug.String() {
		t.Errorf("the convergence logged at %s, want DEBUG", rec["level"])
	}

	// The profile layer must be the one shown holding a single server: that
	// is the whole answer to "why can't this client see s2".
	waitFor(t, "the profile layer's contribution", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, r := range sink.recs {
			if r["msg"] == "scope layer applied" && r["layer"] == "profile" && r["servers"] == "1" {
				return true
			}
		}
		return false
	})
}

// The convergence is a diagnostic and must cost nothing when nobody reads it:
// Explain re-folds the layer list once per layer, so it is real work rather
// than a formatting call slog would discard on its own.
func TestScopeConvergenceIsSkippedBelowDebug(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1")

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	_, c, _ := startGateway(t, Config{
		ClientID: "noconvergence", Resolver: resolver, Log: log,
		Dial: scriptedDial(map[string]*fakemcp.Script{"s1": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo")

	if strings.Contains(buf.String(), "scope layer applied") {
		t.Fatal("the convergence was computed and written at INFO")
	}
	// The Info shape must still be there: the gate covers the detail only, and
	// silencing both would be the wrong half.
	if !strings.Contains(buf.String(), "effective scope resolved") {
		t.Fatalf("the scope shape is missing at INFO: %s", buf.String())
	}
}

// lookupScope returns the scope-shape record logged for one reason. The
// reason is what tells the cold startup line apart from the one describing a
// live catalog, and the two carry different counts on purpose.
func lookupScope(sink *callLog, reason string) (map[string]string, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, r := range sink.recs {
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
