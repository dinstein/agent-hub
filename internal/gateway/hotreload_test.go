package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// notifCount reports how many notifications with method arrived so far.
func (c *testClient) notifCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.notifs {
		if m.Method == method {
			n++
		}
	}
	return n
}

// dialCounter wraps a DialFunc and counts dials per server id — the probe
// for "the downstream connection was NOT dropped" (no re-dial).
type dialCounter struct {
	mu     sync.Mutex
	counts map[string]int
	inner  downstream.DialFunc
}

func newDialCounter(inner downstream.DialFunc) *dialCounter {
	return &dialCounter{counts: map[string]int{}, inner: inner}
}

func (d *dialCounter) fn(ctx context.Context, spec downstream.Spec) (transport.Transport, error) {
	d.mu.Lock()
	d.counts[spec.ID]++
	d.mu.Unlock()
	return d.inner(ctx, spec)
}

func (d *dialCounter) count(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[id]
}

// externalRegistry opens a SECOND store on the gateway's registry dir — an
// external writer whose writes are NOT self-write-suppressed by the
// gateway's watcher.
func externalRegistry(t *testing.T, resolver *platform.Resolver) *registry.Store {
	t.Helper()
	dir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	return store
}

func updateRegistry(t *testing.T, store *registry.Store, fn func(tx *registry.Tx)) {
	t.Helper()
	if err := store.Update(context.Background(), func(tx *registry.Tx) error {
		fn(tx)
		return nil
	}); err != nil {
		t.Fatalf("registry.Update: %v", err)
	}
}

// waitForTools polls tools/list until it equals want (order-insensitive is
// unnecessary: List is sorted).
//
// It does not go through waitFor because the failure message must carry the
// LAST OBSERVED list: waitFor formats its description once, up front, so a
// pointer handed to it renders as the value it had before the first poll —
// which reports every mismatch as "[]" and sends the reader hunting for a
// missing catalog that is actually present but different.
func waitForTools(t *testing.T, c *testClient, want ...string) {
	t.Helper()
	var last []string
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		last = toolNames(c.listTools())
		if slices.Equal(last, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for tools/list == %v; last observed %v", want, last)
}

// callBlockedWithCode asserts a tools/call is rejected and the stable gate
// code appears in the error message.
//
// It retries past codeRetryBusy for the same reason callToolOK does: an
// unknown name while downstreams are still connecting is the RETRYABLE busy
// condition, not a verdict, and a caller that treats it as one is asserting
// on the wrong answer. A test that waits for a downstream's DIAL to be
// recorded has not waited for its handshake, so this window is open on any
// runner slow enough to fit a call into it — which is how it was found.
func callBlockedWithCode(t *testing.T, c *testClient, tool, code string) {
	t.Helper()
	var last *mcp.Error
	waitFor(t, fmt.Sprintf("tools/call %s rejected with %s (last error %v)", tool, code, &last), func() bool {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: tool, Arguments: []byte(`{}`)})
		last = resp.Error
		if resp.Error == nil {
			t.Fatalf("tools/call %s succeeded, want a %s rejection", tool, code)
		}
		if resp.Error.Code == codeRetryBusy && !strings.Contains(resp.Error.Message, code) {
			return false // still connecting: not yet an answer to assert on
		}
		if !strings.Contains(resp.Error.Message, code) {
			t.Fatalf("tools/call %s error %q, want code %s", tool, resp.Error.Message, code)
		}
		return true
	})
}

// callToolOK asserts one successful tools/call round trip for tool,
// retrying the transient busy error while downstreams connect.
func callToolOK(t *testing.T, c *testClient, tool string) {
	t.Helper()
	var lastErr *mcp.Error
	waitFor(t, fmt.Sprintf("tools/call %s (last error %v)", tool, &lastErr), func() bool {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: tool, Arguments: []byte(`{"ok":true}`)})
		lastErr = resp.Error
		if resp.Error == nil {
			return true
		}
		if resp.Error.Code != codeRetryBusy {
			t.Fatalf("tools/call %s: %v", tool, resp.Error)
		}
		return false
	})
}

// TestHotReloadServerAddRemove: an EXTERNAL edit of servers.json makes a
// new server appear (connect + list_changed) and later disappear, while
// the untouched server's downstream connection is never re-dialed
// (docs/flows.md: only the diffed server moves).
func TestHotReloadServerAddRemove(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "alpha")
	dials := newDialCounter(scriptedDial(map[string]*fakemcp.Script{
		"alpha": fakemcp.Minimal("echo"),
		"beta":  fakemcp.Minimal("echo"),
	}))
	_, c, _ := startGateway(t, Config{ClientID: "hot", Resolver: resolver, Dial: dials.fn})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo")

	// External writer adds beta: the new tools must appear and be callable.
	ext := externalRegistry(t, resolver)
	before := c.notifCount(mcp.NotificationToolsListChanged)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Servers.V.Servers["beta"] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "stdio", Command: "unused-in-tests", Enabled: true,
		}}
	})
	waitForTools(t, c, "alpha__echo", "beta__echo")
	callToolOK(t, c, "beta__echo")
	waitFor(t, "list_changed after the add", func() bool {
		return c.notifCount(mcp.NotificationToolsListChanged) > before
	})
	if got := dials.count("alpha"); got != 1 {
		t.Errorf("alpha dialed %d times, want 1 (untouched server must keep its connection)", got)
	}

	// External removal of beta: catalog shrinks, alpha still serves on the
	// SAME connection.
	updateRegistry(t, ext, func(tx *registry.Tx) {
		delete(tx.Servers.V.Servers, "beta")
	})
	waitForTools(t, c, "alpha__echo")
	callToolOK(t, c, "alpha__echo")
	if got := dials.count("alpha"); got != 1 {
		t.Errorf("alpha dialed %d times after the remove, want 1", got)
	}
}

// TestHotReloadProfileNarrowRestore: narrowing the bound profile shrinks
// tools/list (list_changed pushed, calls to the hidden server blocked with
// E_SCOPE_DENIED) WITHOUT touching any downstream connection; restoring
// the profile widens back — again with zero reconnects (docs/architecture.md §7
// invariant 2: visibility is a query-time projection).
func TestHotReloadProfileNarrowRestore(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1", "s2"},
		}}
		tx.Clients.V.Clients["prof-client"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{
			Profile: "team",
		}}
	})

	dials := newDialCounter(scriptedDial(map[string]*fakemcp.Script{
		"s1": fakemcp.Minimal("echo"),
		"s2": fakemcp.Minimal("echo"),
	}))
	_, c, _ := startGateway(t, Config{ClientID: "prof-client", Resolver: resolver, Dial: dials.fn})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "s1__echo", "s2__echo")
	callToolOK(t, c, "s2__echo")

	// Narrow the profile to s1 only.
	before := c.notifCount(mcp.NotificationToolsListChanged)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1"},
		}}
	})
	waitForTools(t, c, "s1__echo")
	waitFor(t, "list_changed after the narrowing", func() bool {
		return c.notifCount(mcp.NotificationToolsListChanged) > before
	})
	// The hidden server is blocked at the scope gate — but its downstream
	// connection is intact (connection plane ≠ visibility plane).
	callBlockedWithCode(t, c, "s2__echo", "E_SCOPE_DENIED")
	callToolOK(t, c, "s1__echo")
	if d1, d2 := dials.count("s1"), dials.count("s2"); d1 != 1 || d2 != 1 {
		t.Errorf("dials after narrowing = (s1 %d, s2 %d), want (1, 1): scope changes must not reconnect", d1, d2)
	}

	// Restore: visibility widens back, still no reconnect.
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1", "s2"},
		}}
	})
	waitForTools(t, c, "s1__echo", "s2__echo")
	callToolOK(t, c, "s2__echo")
	if d1, d2 := dials.count("s1"), dials.count("s2"); d1 != 1 || d2 != 1 {
		t.Errorf("dials after restore = (s1 %d, s2 %d), want (1, 1)", d1, d2)
	}
}

// TestHotReloadServerTraceFlip: turning a server's trace on reaches a gateway
// that is ALREADY RUNNING, without reconnecting it.
//
// The no-reconnect half is the point. Server.trace is captured once at
// Connect, so the only way a later flip can take effect is if the connect
// handed out a real (disabled) log to enable in place — which is why
// traceLogs opens a log for every server rather than only for traced ones.
// A test that merely checked "frames appear" would pass just as well against
// an implementation that silently redialed, and redialing to start logging
// would restart the very server being debugged.
func TestHotReloadServerTraceFlip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolver := testResolver(dir)
	seedRegistry(t, resolver, "alpha")
	dials := newDialCounter(scriptedDial(map[string]*fakemcp.Script{
		"alpha": fakemcp.Minimal("echo"),
	}))
	_, c, _ := startGateway(t, Config{ClientID: "hot", Resolver: resolver, Dial: dials.fn})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "alpha__echo")

	root, err := calllog.DefaultDir(resolver)
	if err != nil {
		t.Fatalf("ledger dir: %v", err)
	}

	// Untraced: a call must leave no FRAMES behind. It does leave lifecycle
	// records — those are the always-on half — so the count is of frames.
	callToolOK(t, c, "alpha__echo")
	if n := frameCount(t, root, "alpha"); n != 0 {
		t.Fatalf("%d frames recorded while tracing is off", n)
	}

	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		doc := tx.Servers.V.Servers["alpha"]
		doc.V.Trace = true
		tx.Servers.V.Servers["alpha"] = doc
	})

	// The flip is observable only through what the next call records, so
	// call until frames appear rather than guessing at the watch latency.
	waitFor(t, "frames after the trace flip", func() bool {
		callToolOK(t, c, "alpha__echo")
		return frameCount(t, root, "alpha") > 0
	})

	if got := dials.count("alpha"); got != 1 {
		t.Errorf("alpha dialed %d times, want 1: enabling a trace must not reconnect the server", got)
	}
}

// frameCount reports how many frames one server has recorded in the ledger.
// A ledger directory that does not exist yet counts as zero: nothing has been
// recorded is a normal state, not an error.
func frameCount(t *testing.T, root, server string) int {
	t.Helper()
	n := 0
	if _, err := calllog.ScanFramesSince(root, time.Time{}, func(e calllog.Event) error {
		if e.Server == server {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("scan frames: %v", err)
	}
	return n
}
