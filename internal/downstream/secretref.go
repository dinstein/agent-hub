package downstream

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// secretPrefix marks a placeholder as a vault reference: ${SECRET_<KEY>}
// resolves the vault entry (spec.ID, spec.ScopeName, <KEY>) — the same
// <KEY> the environment level of the chain reads from
// AGENTHUB_SECRET_<KEY>. The scope component is "_global" for the base
// instance and the derive key for a derived one (docs/modules/dataplane.md).
const secretPrefix = "SECRET_"

// ErrUnresolvedSecret reports a ${SECRET_X} placeholder with no value in
// the vault.
var ErrUnresolvedSecret = errors.New("downstream: unresolved secret placeholder")

// ErrNoResolver reports a spec that references a secret while no resolver
// was injected.
var ErrNoResolver = errors.New("downstream: secret placeholder used but no secrets resolver is wired")

// expandSecrets substitutes every ${SECRET_X} placeholder in s against the
// vault, returning the resolved string.
//
// Failure direction: FAIL-CLOSED, in both directions that matter.
//
//   - An unresolved placeholder is an ERROR, never a passthrough. Sending
//     the literal text "${SECRET_GITHUB_TOKEN}" as an Authorization header
//     produces a 401 that looks exactly like an expired token, and the
//     operator debugs the wrong problem; worse, a header expanded to the
//     empty string can turn an authenticated endpoint into an anonymous one.
//   - The error names the KEY, never the value, and no resolved value is
//     ever logged or embedded in an error (docs/modules/controlplane.md rule 5: secrets are
//     never echoed).
//
// Placeholders that do not start with SECRET_ are left verbatim: they belong
// to other substitution layers (${ROOT} et al.) or are literal text a
// downstream expects to receive unchanged.
func expandSecrets(ctx context.Context, serverID, scopeName, s string, resolve secrets.Resolver) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "${")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		end := strings.Index(rest[i:], "}")
		if end < 0 {
			// Unterminated: not a placeholder at all, keep it verbatim.
			b.WriteString(rest)
			return b.String(), nil
		}
		end += i
		name := rest[i+2 : end]
		b.WriteString(rest[:i])
		if !strings.HasPrefix(name, secretPrefix) {
			b.WriteString(rest[i : end+1]) // not ours; verbatim
			rest = rest[end+1:]
			continue
		}
		key := strings.TrimPrefix(name, secretPrefix)
		if strings.TrimSpace(key) == "" {
			return "", fmt.Errorf("downstream %q: empty secret placeholder ${%s}", serverID, name)
		}
		if resolve == nil {
			return "", fmt.Errorf("%w (server %q, key %q)", ErrNoResolver, serverID, key)
		}
		val, ok, err := resolveScoped(ctx, serverID, scopeName, key, resolve)
		if err != nil {
			// The vault failed (bad master key, broken keychain). Surfacing
			// it beats silently degrading to "not set".
			return "", fmt.Errorf("downstream %q: resolve secret %q: %w", serverID, key, err)
		}
		if !ok || val == "" {
			return "", fmt.Errorf("%w: server %q needs secret %q", ErrUnresolvedSecret, serverID, key)
		}
		b.WriteString(val)
		rest = rest[end+1:]
	}
}

// resolveScoped reads one vault entry under the instance's scope, falling
// back to the "_global" entry when the scope has no value of its own.
//
// The fallback is what makes derived instances usable at all: an operator
// stores GITHUB_TOKEN once, and every root-derived instance inherits it;
// storing a value under a specific scope then OVERRIDES it for exactly that
// derivation (the per-scope identity of docs/modules/dataplane.md). The order is
// specific-wins, and a vault ERROR at either level aborts — a broken
// keychain must never silently degrade a scoped credential into the shared
// one.
func resolveScoped(ctx context.Context, serverID, scopeName, key string, resolve secrets.Resolver) (string, bool, error) {
	if scopeName != "" && scopeName != secrets.DefaultScope {
		val, ok, err := resolve(ctx, secrets.Ref{ServerID: serverID, Scope: scopeName, Key: key})
		if err != nil {
			return "", false, err
		}
		if ok && val != "" {
			return val, true, nil
		}
	}
	return resolve(ctx, secrets.UserRef(serverID, key))
}

// expandSecretMap expands every value of m. Keys are never expanded (an env
// variable name or a header name is not a credential) and the input map is
// left untouched. Iteration is sorted so a failure always reports the same
// key first for a given input — determinism is contract for error golden
// tests.
func expandSecretMap(ctx context.Context, serverID, scopeName string, m map[string]string, resolve secrets.Resolver) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		v, err := expandSecrets(ctx, serverID, scopeName, m[k], resolve)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}
