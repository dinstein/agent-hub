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

	declared := parseNotificationSet(t,
		filepath.Join(root, "internal", "httpbridge", "stream.go"), "carriedNotifications")
	if len(declared) == 0 {
		t.Fatal("no carriedNotifications entries found; this test asserted nothing")
	}
	produced := gatewayUpstreamNotifications(t, filepath.Join(root, "internal", "gateway"))
	if len(produced) == 0 {
		t.Fatal("no upstream notification producers found in internal/gateway; this test asserted nothing")
	}
	// The gateway keeps the same set for the stdio face's own filter, so
	// there are three things to hold together, not two.
	gatewaySet := parseNotificationSet(t,
		filepath.Join(root, "internal", "gateway", "subscriptions.go"), "producedNotifications")
	if len(gatewaySet) == 0 {
		t.Fatal("no producedNotifications entries found; this test asserted nothing")
	}
	for name := range produced {
		if !gatewaySet[name] {
			t.Errorf("internal/gateway sends mcp.%s upstream, and its own producedNotifications "+
				"does not name it.\n"+
				"The stdio face's acknowledgement then tells a subscriber the type is unsupported, "+
				"and honouredFilter drops it — so a client that subscribed correctly stops "+
				"receiving a notification this gateway does send.", name)
		}
	}
	for name := range gatewaySet {
		if !produced[name] {
			t.Errorf("internal/gateway's producedNotifications names mcp.%s, which nothing in the "+
				"package sends upstream.\n"+
				"Drop it, or wire the producer.", name)
		}
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

// carriedEntry matches a `mcp.NotificationX: true,` line inside one of the
// declared map literals.
var carriedEntry = regexp.MustCompile(`^\s*mcp\.(Notification\w+):\s*true,`)

// parseNotificationSet reads one `var <name> = map[string]bool{...}` literal.
func parseNotificationSet(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	inMap := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "var "+name+" = map[string]bool{") {
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

// notSubscribable names notifications that are produced upstream but are not
// SUBSCRIBABLE, so they belong in neither declared set.
//
// One member: the subscription's own acknowledgement. It is the subscription
// mechanism talking about itself — a client cannot subscribe to it, and it
// must be sent regardless of any filter, since it is what tells the client
// what the filter ended up being. Putting it in the sets would offer clients
// a subscription to the message that answers subscriptions.
var notSubscribable = map[string]bool{
	"NotificationSubscriptionsAcknowledged": true,
}

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
			if notSubscribable[m[1]] {
				continue
			}
			out[m[1]] = true
		}
	}
	return out
}
