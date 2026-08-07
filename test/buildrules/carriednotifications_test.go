package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCarriedNotificationsMatchTheGateway pins an agreement between two
// packages that cannot see each other's decision.
//
// `internal/httpbridge` declares `carriedNotifications` — what a 2026-07-28
// stream will actually deliver — and intersects a client's subscription
// filter with it. What that set has to equal is the set of notifications
// `internal/gateway` ever sends UPSTREAM, which today is one:
// `notifications/tools/list_changed`.
//
// The failure this catches is invisible in every other way. Add a producer to
// the gateway without adding it here and the code compiles, the tests pass,
// and the new notification is silently filtered out of every 2026 stream —
// while the acknowledgement actively TELLS the client it will never arrive,
// so the client is right to stop waiting. That is worse than a dropped
// message: it is a dropped message the protocol confirmed.
//
// The reverse direction is checked too. A method listed here that nothing
// produces makes the acknowledgement promise a type no client will ever see,
// which is the same silence the acknowledgement exists to break.
func TestCarriedNotificationsMatchTheGateway(t *testing.T) {
	root := repoRoot(t)

	declared := parseCarriedNotifications(t, filepath.Join(root, "internal", "httpbridge", "stream.go"))
	if len(declared) == 0 {
		t.Fatal("no carriedNotifications entries found; this test asserted nothing")
	}
	produced := gatewayUpstreamNotifications(t, filepath.Join(root, "internal", "gateway"))
	if len(produced) == 0 {
		t.Fatal("no upstream notification producers found in internal/gateway; this test asserted nothing")
	}

	for name := range produced {
		if !declared[name] {
			t.Errorf("internal/gateway sends mcp.%s upstream, and internal/httpbridge's "+
				"carriedNotifications does not name it.\n"+
				"A 2026-07-28 stream will filter it out, AND the acknowledgement will tell the "+
				"client it is not supported — so the client stops waiting for a notification the "+
				"hub does produce.\n"+
				"Add it to carriedNotifications, and to acceptFromFilter's switch.", name)
		}
	}
	for name := range declared {
		if !produced[name] {
			t.Errorf("internal/httpbridge's carriedNotifications names mcp.%s, which "+
				"internal/gateway never sends upstream.\n"+
				"The acknowledgement then promises a type that never arrives, which is exactly "+
				"the silence it exists to break.\n"+
				"Drop it, or wire the producer.", name)
		}
	}
}

// carriedEntry matches a `mcp.NotificationX: true,` line inside the
// carriedNotifications map literal.
var carriedEntry = regexp.MustCompile(`^\s*mcp\.(Notification\w+):\s*true,`)

func parseCarriedNotifications(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	inMap := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "var carriedNotifications = map[string]bool{") {
			inMap = true
			continue
		}
		if inMap {
			if strings.HasPrefix(line, "}") {
				break
			}
			if m := carriedEntry.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

// gatewayNotify matches the gateway's one way of sending a notification to
// its client: g.reply(mcp.NewNotification(mcp.NotificationX, ...)).
//
// Deliberately narrow. `reply` is the upstream write, and a notification
// built for any other purpose — forwarded downstream, constructed in a test —
// is not what this rule is about.
var gatewayNotify = regexp.MustCompile(`reply\(mcp\.NewNotification\(mcp\.(Notification\w+)`)

func gatewayUpstreamNotifications(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range gatewayNotify.FindAllStringSubmatch(string(data), -1) {
			out[m[1]] = true
		}
	}
	return out
}
