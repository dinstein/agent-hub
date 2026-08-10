package gateway

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/scope"
)

// brokenRegistryResolver returns a resolver whose registry directory is a
// regular file, so registry.Open's MkdirAll fails and the gateway starts with
// g.store == nil — the condition the finding is about. The cache directory is
// left usable, and one cached "secret" server is seeded so there is a catalog
// to (not) disclose.
func brokenRegistryResolver(t *testing.T) *platform.Resolver {
	t.Helper()
	dir := t.TempDir()
	resolver := testResolver(dir)

	regDir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	// RegistryDir may have created the directory; replace it with a file so
	// registry.Open cannot use it.
	_ = os.RemoveAll(regDir)
	if err := os.MkdirAll(filepath.Dir(regDir), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(regDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("plant file at registry dir: %v", err)
	}

	cacheDir, err := resolver.CacheDir()
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	cache := newToolCache(filepath.Join(cacheDir, toolCacheSubdir), slog.New(slog.DiscardHandler))
	if err := cache.write("secret", []mcp.ToolDef{{
		Name:        "peek",
		Description: "peek at the secret server",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	return resolver
}

// searchFindsSecretPeek drives the lazy-mode search_tools meta-tool (store==nil
// defaults discovery to lazy) and reports whether the cached secret tool was
// surfaced to this caller.
func searchFindsSecretPeek(t *testing.T, c *testClient) bool {
	t.Helper()
	res := callToolResult(t, c, discovery.MetaSearchTools, map[string]any{"query": "peek secret"})
	if res.IsError {
		t.Fatalf("search_tools error: %s", resultText(t, res))
	}
	if len(res.StructuredContent) == 0 {
		return false // an empty surface returns no structured results
	}
	var payload struct {
		Results []struct {
			Tool string `json:"tool"`
		} `json:"results"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode search payload: %v\n%s", err, res.StructuredContent)
	}
	for _, r := range payload.Results {
		if r.Tool == "secret__peek" {
			return true
		}
	}
	return false
}

// TestScopeFailsClosedWhenRegistryUnavailable is the regression for the
// 2026-08-10 sweep's high finding. When registry.Open fails the gateway has no
// scope store, and the credential's narrowing layers (an HTTP agent token's
// server allowlist) were wired only inside `if g.store != nil` — so a restricted
// token fell through to the uncredentialed full-cache baseline and could see
// every cached server's catalog. A narrowing credential with no authority to
// resolve against must fail closed (empty scope), not widen.
func TestScopeFailsClosedWhenRegistryUnavailable(t *testing.T) {
	t.Run("restricted token cannot see the cached catalog", func(t *testing.T) {
		resolver := brokenRegistryResolver(t)
		_, c, _ := startGateway(t, Config{
			ClientID: "pinned-token",
			Resolver: resolver,
			ScopeLayers: func() []scope.ScopeLayer {
				return []scope.ScopeLayer{{
					Kind:    scope.LayerSession,
					Origin:  "token:pinned-token",
					Servers: []string{"some-allowed-server"}, // not "secret"
				}}
			},
		})
		c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
		if searchFindsSecretPeek(t, c) {
			t.Fatal("a token allowlisting another server was shown the cached secret tool")
		}
	})

	t.Run("uncredentialed session keeps the full-cache baseline", func(t *testing.T) {
		resolver := brokenRegistryResolver(t)
		_, c, _ := startGateway(t, Config{
			ClientID: "local-stdio",
			Resolver: resolver,
			// No ScopeLayers: the pre-M1 baseline must be unchanged, so the fix
			// does not over-fire on the ordinary local client.
		})
		c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
		if !searchFindsSecretPeek(t, c) {
			t.Fatal("an uncredentialed session lost the full-cache baseline")
		}
	})
}
