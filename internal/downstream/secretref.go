package downstream

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// secretPrefix marks a placeholder as a vault reference: ${SECRET_<KEY>}
// resolves the vault entry (spec.ID, spec.ScopeName, <KEY>) — the same
// <KEY> the environment level of the chain reads from
// AGENTHUB_SECRET_<KEY>. The scope component is "_global" for the base
// instance and the derive key for a derived one (docs/subsystems/downstream.md).
const secretPrefix = "SECRET_"

// ErrUnresolvedSecret reports a ${SECRET_X} placeholder with no value in
// the vault.
var ErrUnresolvedSecret = errors.New("downstream: unresolved secret placeholder")

// UnresolvedSecretError identifies the vault entry a server definition needs
// without exposing its value. Callers may use errors.As to turn a failed dial
// into a setup action; they must not recover this information by parsing the
// human error text.
type UnresolvedSecretError struct {
	ServerID string
	Key      string
}

func (e *UnresolvedSecretError) Error() string {
	return fmt.Sprintf("%v: server %q needs secret %q", ErrUnresolvedSecret, e.ServerID, e.Key)
}

func (e *UnresolvedSecretError) Unwrap() error { return ErrUnresolvedSecret }

// connectFailure classifies a failed dial into the event kind that says what
// the operator has to DO about it.
//
// A missing secret and a refused TCP connection both surface as "this server
// did not connect", and they send someone to opposite places: one is a setup
// step that was never finished, the other is a server or a network. The
// closed vocabulary exists so that difference survives into a timeline, and
// the classification reads the typed error rather than the message text, for
// the reason UnresolvedSecretError states — the human wording is not an API.
//
// The Detail is the error, which for an unresolved secret names the KEY and
// never the value (docs/subsystems/docs/subsystems/controlplane.md rule 5).
func connectFailure(err error) eventlog.Record {
	kind := eventlog.KindConnectFailed
	var missing *UnresolvedSecretError
	switch {
	case errors.As(err, &missing):
		kind = eventlog.KindSecretsMissing
	case errors.Is(err, ErrUnresolvedSecret), errors.Is(err, ErrNoResolver):
		// Same cause reached without the typed error: a resolver that was
		// never wired blocks on exactly the same setup step.
		kind = eventlog.KindSecretsMissing
	}
	return eventlog.Record{Kind: kind, Detail: err.Error()}
}

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
//     ever logged or embedded in an error (docs/subsystems/docs/subsystems/controlplane.md rule 5: secrets are
//     never echoed).
//
// Placeholders that do not start with SECRET_ are left verbatim: they belong
// to other substitution layers (${ROOT} et al.) or are literal text a
// downstream expects to receive unchanged.
func expandSecrets(ctx context.Context, serverID, scopeName, s string, resolve secrets.Resolver) (string, error) {
	found, tail := scanPlaceholders(s)
	if len(found) == 0 {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, p := range found {
		b.WriteString(p.before)
		key, ours := p.secretKey()
		if !ours {
			b.WriteString(p.verbatim())
			continue
		}
		if strings.TrimSpace(key) == "" {
			return "", fmt.Errorf("downstream %q: empty secret placeholder %s", serverID, p.verbatim())
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
			return "", &UnresolvedSecretError{ServerID: serverID, Key: key}
		}
		b.WriteString(val)
	}
	b.WriteString(tail)
	return b.String(), nil
}

// placeholder is one ${…} occurrence found by scanPlaceholders: the literal
// text since the previous occurrence, and the name between the braces.
type placeholder struct {
	before string
	name   string
}

// secretKey reports the vault key this placeholder names, and whether it is
// ours at all. Only the SECRET_ prefix is; the remainder is the key, spelled
// the same way the environment level of the chain reads it from
// AGENTHUB_SECRET_<KEY>.
func (p placeholder) secretKey() (string, bool) {
	if !strings.HasPrefix(p.name, secretPrefix) {
		return "", false
	}
	return strings.TrimPrefix(p.name, secretPrefix), true
}

// verbatim renders the placeholder exactly as it appeared, which is what a
// placeholder belonging to another substitution layer (${ROOT} et al.) is
// worth to this one.
func (p placeholder) verbatim() string { return "${" + p.name + "}" }

// scanPlaceholders splits s at its ${…} occurrences and returns them together
// with the literal tail after the last one. A string with no placeholder
// yields no occurrences and the whole of s as tail.
//
// An opening "${" with no closing brace is not a placeholder at all: the scan
// stops there, so everything from it onwards is tail and reaches the output
// unchanged.
//
// One scanner for the two readers below it — expandSecrets, which substitutes,
// and SecretKeysIn, which reports what would be substituted. The two must
// agree on where a placeholder starts, where it ends and what its name is, and
// while each carried its own copy of those three rules only a test held them
// together. A rule nothing fails on is a rule that drifts.
func scanPlaceholders(s string) (found []placeholder, tail string) {
	rest := s
	for {
		i := strings.Index(rest, "${")
		if i < 0 {
			return found, rest
		}
		end := strings.Index(rest[i:], "}")
		if end < 0 {
			return found, rest
		}
		end += i
		found = append(found, placeholder{before: rest[:i], name: rest[i+2 : end]})
		rest = rest[end+1:]
	}
}

// SecretKeysIn reports the vault KEYS named by the ${SECRET_<KEY>}
// placeholders of s, in order of appearance and with the prefix stripped —
// the read-only half of expandSecrets, for callers that need to know what an
// entry REQUIRES without resolving anything (the CLI's `server inspect`
// inventory and the credential column of `server ls`).
//
// It exists so that "which secrets does this server need" has ONE answer.
// Deriving that list from a private ${...} scan somewhere else is how a
// listing ends up naming ${ROOT} as a missing credential while the real
// ${SECRET_GITHUB_TOKEN} goes unmentioned: both rules — the SECRET_ prefix
// and the stripping — belong to this file, and a second implementation of
// them drifts silently because nothing fails when it does. Which is why the
// scan itself is shared (scanPlaceholders) and only the two answers differ.
//
// TestSecretKeysInMatchesExpansion pins those answers together.
func SecretKeysIn(s string) []string {
	found, _ := scanPlaceholders(s)
	var out []string
	for _, p := range found {
		key, ours := p.secretKey()
		if !ours {
			continue // another substitution layer owns it
		}
		// An empty ${SECRET_} is a configuration error expandSecrets rejects at
		// dial time. Naming "" as a required key here would be useless, so it
		// is left to the dial to report.
		if strings.TrimSpace(key) != "" {
			out = append(out, key)
		}
	}
	return out
}

// resolveScoped reads one vault entry under the instance's scope, falling
// back to the "_global" entry when the scope has no value of its own.
//
// The fallback is what makes derived instances usable at all: an operator
// stores GITHUB_TOKEN once, and every root-derived instance inherits it;
// storing a value under a specific scope then OVERRIDES it for exactly that
// derivation (the per-scope identity of docs/subsystems/downstream.md). The order is
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
	keys := slices.Sorted(maps.Keys(m))
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
