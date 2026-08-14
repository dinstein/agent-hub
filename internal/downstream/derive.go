package downstream

import (
	"fmt"
	"maps"
	"strings"
)

// This file is the CONNECTION-PLANE half of derived downstream instances
// (docs/modules/dataplane.md). A derived instance is the same registry server dialed
// with session-specialized connection parameters (cwd / ${ROOT} / env);
// everything else about it — exposed tool names, routing, visibility — is
// unchanged.
//
// Two invariants make that statement true and are the reason the derivation
// lives here rather than in the router or the scope layer:
//
//  1. Spec.ID NEVER changes. The derived spec keeps the base server id, so
//     router.RouteOf stays the single provenance of a call, scope
//     intersections keep matching by (serverID, rawTool), and audit records
//     name the server the operator configured. Only DeriveKey distinguishes
//     the instances, and it is a CONNECTION-plane key: which process a call
//     is executed on. Deriving never adds, removes or renames a tool
//     (docs/model.md invariant 2: connection plane and visibility plane are
//     separate).
//
//  2. The derive key IS the vault scope name (Spec.ScopeName). The
//     composite vault key (serverID, scopeName) was pulled forward into M1
//     precisely for this moment: a derived instance resolves its
//     ${SECRET_X} placeholders and its bearer credential under its own
//     scope, falling back to the "_global" entry when that scope has no
//     value of its own (see secretref.go). Without the fallback every
//     derivation would need its secrets re-entered; without the scoped
//     lookup a per-root identity could never exist.

// DeriveMode is the per-server derivation policy from the registry
// (ServerEntry.Derive). An entry written before this field existed derives
// nothing — backward compatible by construction — but that normalization is
// ParseDeriveMode's and not the type's: the zero DeriveMode is "", which does
// NOT compare equal to DeriveNone. This said the opposite, and a caller acted
// on it, testing both spellings because the sentence left it unable to say
// which one it would be handed.
//
// Every Spec on the connection plane comes from SpecFromEntry, which parses,
// so "" never travels with one: compare against DeriveNone and nothing else.
type DeriveMode string

// The three derivation modes.
const (
	// DeriveNone: one instance per server, shared by every session. The
	// default and the only pre-M2 behaviour.
	DeriveNone DeriveMode = "none"
	// DeriveRoot: one instance per project root. Sessions sharing a root
	// share the instance — which is the point: two Cursor windows on the
	// same repository must not spawn two language servers.
	DeriveRoot DeriveMode = "root"
	// DeriveSession: one instance per session. The strongest isolation and
	// the most expensive one; reserved for servers whose per-session state
	// genuinely cannot be shared.
	DeriveSession DeriveMode = "session"
)

// ParseDeriveMode validates the on-disk spelling. An empty value is
// DeriveNone (the backward-compatible default); anything unknown is an
// ERROR rather than a silent fallback — a typo in `derive` must not
// quietly collapse an isolation requirement into shared state.
func ParseDeriveMode(s string) (DeriveMode, error) {
	switch DeriveMode(strings.TrimSpace(s)) {
	case "", DeriveNone:
		return DeriveNone, nil
	case DeriveRoot:
		return DeriveRoot, nil
	case DeriveSession:
		return DeriveSession, nil
	default:
		return DeriveNone, fmt.Errorf("downstream: unknown derive mode %q (want %q, %q or %q)",
			s, DeriveNone, DeriveRoot, DeriveSession)
	}
}

// DeriveKey identifies one derivation of a server. The empty key means "the
// base instance" — the shared connection every non-deriving session uses.
//
// Keys are readable rather than hashed on purpose: they appear in logs, in
// `server ls` annotations and — as Spec.ScopeName — in vault storage keys,
// where an opaque digest would make a per-scope credential impossible to
// administer. The prefixes keep the two namespaces apart, so a root named
// like a session id can never collide with one.
type DeriveKey string

// Key prefixes. Frozen: they are part of the vault scope name, and changing
// one orphans every secret stored under the old spelling.
const (
	rootKeyPrefix    = "root:"
	sessionKeyPrefix = "session:"
)

// RootDeriveKey builds the key of a root-derived instance. root should
// already be normalized by the caller (scope.NormalizePath); the light
// normalization applied here is idempotent over that output and exists so a
// caller that cannot import internal/scope — this package cannot, it would
// close the cycle downstream → scope → router → downstream — still produces
// the same key for the same directory.
//
// An empty root yields an empty key: there is nothing to specialize on, and
// the caller must then use the base instance rather than invent a variant
// keyed by "".
func RootDeriveKey(root string) DeriveKey {
	n := normalizeRoot(root)
	if n == "" {
		return ""
	}
	return DeriveKey(rootKeyPrefix + n)
}

// SessionDeriveKey builds the key of a session-derived instance. An empty
// session id yields an empty key (base instance) for the same reason as
// above.
func SessionDeriveKey(sessionID string) DeriveKey {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return ""
	}
	return DeriveKey(sessionKeyPrefix + id)
}

// normalizeRoot is the string-only path normalization used for key
// building: separators unified, runs collapsed, trailing separator dropped,
// Windows paths lowercased. It NEVER touches the disk and never resolves
// symlinks — the path is a claim by a client about its own machine, and on
// this machine it may not even exist (docs/model.md, the same rule
// scope.NormalizePath follows).
func normalizeRoot(p string) string {
	s := strings.TrimSpace(p)
	if s == "" {
		return ""
	}
	windows := strings.ContainsRune(s, '\\') ||
		(len(s) >= 2 && s[1] == ':' && isASCIIAlpha(s[0]))
	s = strings.ReplaceAll(s, "\\", "/")

	var b strings.Builder
	b.Grow(len(s))
	prevSlash := false
	for _, r := range s {
		if r == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteRune(r)
	}
	s = b.String()
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	if windows {
		s = strings.ToLower(s)
	}
	return s
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// rootPlaceholder is expanded in Args, Env values and Cwd of a derived
// spec. It is the same spelling clients.json uses for explicit roots.
//
// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28): the value comes
// from the MCP roots capability when the client offers one; the placeholder
// itself outlives it (clients.json carries explicit roots too).
const rootPlaceholder = "${ROOT}"

// DeriveContext carries the per-derivation inputs. Root feeds ${ROOT}
// expansion; Env is the explicit per-derivation override applied on top of
// the base environment (the `--derive-server github --env GITHUB_ORG=acme`
// CLI seam of docs/modules/dataplane.md).
type DeriveContext struct {
	Root string
	Env  map[string]string
}

// Derived returns the specialized spec of one derivation. The receiver is
// never mutated and never aliased by the result: every map and slice is
// cloned, so a derived instance cannot write through into the base spec the
// registry loaded.
//
// What is specialized: Args, Env VALUES and Cwd (${ROOT} expansion, then
// the explicit Env overlay). What is deliberately NOT: URL and Headers. A
// header patch needs no new connection — the per-call RoundTripper injects
// it (docs/modules/dataplane.md "headers-only fast path") — and spending a process on one
// would be the expensive answer to a cheap question.
//
// Secret placeholders are left VERBATIM here (as everywhere outside dial
// time) and are resolved under Spec.ScopeName when the instance connects.
func (s Spec) Derived(key DeriveKey, dc DeriveContext) Spec {
	out := s
	out.DeriveKey = key
	out.ScopeName = string(key)
	out.Args = expandArgs(s.Args, dc.Root)
	out.Cwd = expandRoot(s.Cwd, dc.Root)
	out.Env = deriveEnv(s.Env, dc)
	out.Headers = maps.Clone(s.Headers)
	return out
}

// InstanceID names one instance for logs and diagnostics: the server id for
// the base instance, "<id>#<key>" for a derivation. It is a DISPLAY string
// — never a routing key, never a vault component.
func (s Spec) InstanceID() string {
	if s.DeriveKey == "" {
		return s.ID
	}
	return s.ID + "#" + string(s.DeriveKey)
}

func deriveEnv(base map[string]string, dc DeriveContext) map[string]string {
	if len(base) == 0 && len(dc.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(dc.Env))
	for k, v := range base {
		out[k] = expandRoot(v, dc.Root)
	}
	for k, v := range dc.Env {
		out[k] = expandRoot(v, dc.Root)
	}
	return out
}

func expandArgs(args []string, root string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandRoot(a, root)
	}
	return out
}

// expandRoot substitutes ${ROOT}. An EMPTY root leaves the placeholder
// verbatim instead of expanding to "": a command line that silently becomes
// `--project ` (or worse, a cwd of "") would run against the wrong
// directory, while an unexpanded placeholder fails loudly at spawn.
func expandRoot(s, root string) string {
	if root == "" || !strings.Contains(s, rootPlaceholder) {
		return s
	}
	return strings.ReplaceAll(s, rootPlaceholder, root)
}
