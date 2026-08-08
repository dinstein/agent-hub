package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// netguard is the other of the two layers AGENTS.md puts outside the
// permission model — "netguard / spawnguard refuse destinations and processes
// regardless of who asked" — and spawnguard got its first e2e last round.
// This one had a single case, `TestHTTPDownstreamRefusedWithoutLocalProvenance`,
// covering one address shape at one of the two enforcement points.
//
// The rule has a shape that a single case cannot show, and it is the shape an
// operator meets: the refusal comes with a hint suggesting `--local`, and
// `--local` deliberately does NOT unblock most of what gets refused. It
// launders a literal loopback URL and nothing else — never RFC1918, never
// link-local — because those are the ranges cloud metadata services and
// intranet hosts live in, and an SSRF that talks the operator into passing a
// flag is still an SSRF.
//
// The second enforcement point is the one that actually holds the line. The
// CLI check exists so the operator finds out while they can still fix it; the
// CONNECTOR screens independently, which is why the last case here writes a
// configuration the CLI would have refused and asserts the gateway refuses it
// anyway. Without that case, deleting the connector's screen would leave every
// other test in this file green.

// privateURLs are the destinations an SSRF aims at, one per reason.
var privateURLs = []struct {
	name, url, why string
}{
	{"rfc1918", "http://10.1.2.3:9/mcp", "an intranet host"},
	{"link-local metadata", "http://169.254.169.254/mcp", "the cloud metadata service"},
	{"loopback", "http://127.0.0.1:9/mcp", "this machine"},
}

// TestAPrivateEndpointIsRefusedWhenAdded covers the add-time screen across
// every shape, not just the loopback one the existing case uses.
//
// Loopback is the friendly mistake and the other two are the dangerous ones,
// so a test that only covered loopback would be covering the shape whose
// refusal matters least.
func TestAPrivateEndpointIsRefusedWhenAdded(t *testing.T) {
	for _, tc := range privateURLs {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			code, out := runAgenthubExit(t, dataDir, "", "server", "add", "target",
				"--url", tc.url, "--transport", "http", "--json")
			if code == 0 {
				t.Fatalf("adding %s (%s) succeeded: %s", tc.url, tc.why, out)
			}
			if !strings.Contains(out, "private address") {
				t.Errorf("the refusal does not say why: %s", out)
			}
		})
	}
}

// TestLocalDoesNotLaunderANonLoopbackAddress is the case the hint makes
// necessary.
//
// Every refusal above that involves loopback suggests `--local`, so an
// operator who hits the RFC1918 or metadata refusal has been shown a flag
// that looks like the way through. It is not, and the narrowness is the whole
// value of the carve-out: a flag that unblocked 169.254.169.254 would turn
// one social-engineering step into a read of the cloud credentials.
func TestLocalDoesNotLaunderANonLoopbackAddress(t *testing.T) {
	for _, tc := range privateURLs {
		if tc.name == "loopback" {
			continue // the one address --local exists for; asserted below
		}
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			code, out := runAgenthubExit(t, dataDir, "", "server", "add", "target",
				"--url", tc.url, "--transport", "http", "--local", "--json")
			if code == 0 {
				t.Fatalf("--local laundered %s (%s): %s", tc.url, tc.why, out)
			}
			if !strings.Contains(out, "loopback") {
				t.Errorf("the refusal does not explain how narrow --local is: %s", out)
			}
		})
	}

	// And the control: --local does work for the address it exists for, so
	// the refusals above are about narrowness rather than the flag being
	// broken.
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "target",
		"--url", "http://127.0.0.1:9/mcp", "--transport", "http", "--local", "--json")
}

// TestTheSameScreenGuardsTheOAuthEndpointPins covers the second call site of
// the same predicate, which is where a rule like this gets forgotten.
//
// An OAuth pin is a URL the login flow will fetch, so it reaches the network
// exactly as `--url` does; screening one and not the other would leave the
// hole open through a flag nobody thinks of as an endpoint. These pins had no
// e2e at all.
func TestTheSameScreenGuardsTheOAuthEndpointPins(t *testing.T) {
	for _, flag := range []string{"--oauth-issuer", "--oauth-resource-metadata"} {
		t.Run(flag, func(t *testing.T) {
			dataDir := t.TempDir()
			code, out := runAgenthubExit(t, dataDir, "", "server", "add", "target",
				"--url", "https://example.invalid/mcp", "--transport", "http",
				flag, "https://169.254.169.254/.well-known/oauth-authorization-server",
				"--json")
			if code == 0 {
				t.Fatalf("%s accepted the cloud metadata address: %s", flag, out)
			}
			if !strings.Contains(out, "private address") {
				t.Errorf("the refusal does not say why: %s", out)
			}
		})
	}
}

// writeServersJSON writes registry/servers.json directly — the operator path
// a hand edit, a migration or a synced dotfile takes, and the one no CLI
// validation stands in front of.
func writeServersJSON(t *testing.T, dataDir string, doc map[string]any) {
	t.Helper()
	dir := filepath.Join(dataDir, "registry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheConnectorRefusesAPrivateEndpointTheCLINeverSaw is the case that
// makes the rest of this file mean something.
//
// Everything above goes through `server add`, so all of it would still pass if
// the screen existed ONLY there — and a config file is not always written by
// the CLI. It arrives by hand edit, by migration, by a dotfile synced from
// another machine. This one writes the entry the CLI would have refused,
// marked `provenance: local` so that even the carve-out is claimed, and
// asserts the gateway refuses to dial it anyway.
//
// That is the documented division: the CLI check is there so an operator
// finds out while they can still fix it, and the connector is the boundary.
func TestTheConnectorRefusesAPrivateEndpointTheCLINeverSaw(t *testing.T) {
	dataDir := t.TempDir()

	// A working stdio server alongside, so the gateway is demonstrably past
	// its dialling phase before the absence of the other one is read as a
	// refusal.
	runAgenthub(t, dataDir, "", "server", "add", "bystander", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "bystander")

	// Now overwrite the registry with an entry no CLI would have written:
	// a metadata-service address wearing the local carve-out.
	writeServersJSON(t, dataDir, map[string]any{
		"servers": map[string]any{
			"bystander": map[string]any{
				"enabled": true, "source": "manual", "transport": "stdio",
				"command": fakemcpBin,
			},
			"smuggled": map[string]any{
				"enabled": true, "source": "manual", "transport": "http",
				"url":        "http://169.254.169.254/mcp",
				"provenance": "local",
			},
		},
	})

	c := startGateway(t, dataDir, "netguardclient")
	c.initialize()
	c.waitForTool("bystander__echo", 30*time.Second)

	if names := c.listTools(30 * time.Second); slices.Contains(names, "smuggled__echo") {
		c.fatalf("the connector dialled a link-local address: %v", names)
	}
	// THE LOG LINE IS THE REAL ASSERTION, and the absence above is not. That
	// address is unreachable from a test machine, so a hub with no screen at
	// all would also fail to serve the tool — the tool being missing proves
	// nothing on its own. What separates the two is that the refusal names
	// the narrowness and arrives before any network I/O, where a bare failed
	// dial would report a connection error instead.
	waitStderr(t, c, 30*time.Second, "smuggled", "loopback")
	c.close()
}
