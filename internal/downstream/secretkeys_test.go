package downstream

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// TestSecretKeysInMatchesExpansion is the anti-drift pin between the two
// halves of the placeholder rule: whatever expandSecrets actually LOOKS UP in
// the vault is exactly what SecretKeysIn reports, for every shape of input.
//
// Without it the inventory and the resolver are free to disagree, and the
// disagreement is invisible: a listing that names ${ROOT} as a required
// secret, or that omits a real one, still passes every test either side has
// of its own.
func TestSecretKeysInMatchesExpansion(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"plain text",
		"${SECRET_A}",
		"pre ${SECRET_A} mid ${SECRET_B} post",
		"${ROOT}/workspace",
		"${SECRET_A}${ROOT}${SECRET_A}",
		"Bearer ${SECRET_TOKEN}",
		"${unterminated",
		"${SECRET_A",
		"}{",
		"$",
		"${}",
		"${SECRET_A}${}",
	} {
		var asked []string
		resolve := func(_ context.Context, ref secrets.Ref) (string, bool, error) {
			asked = append(asked, ref.Key)
			return "value", true, nil
		}
		_, err := expandSecrets(context.Background(), "srv", secrets.DefaultScope, in, resolve)
		keys := SecretKeysIn(in)
		if err != nil {
			// The only inputs expandSecrets rejects are empty placeholders,
			// which name no key at all. What matters here is that the
			// inventory does not invent one.
			if slices.Contains(keys, "") {
				t.Errorf("SecretKeysIn(%q) = %v, must never report an empty key", in, keys)
			}
			continue
		}
		if !slices.Equal(keys, asked) {
			t.Errorf("SecretKeysIn(%q) = %v, but expansion looked up %v", in, keys, asked)
		}
	}
}

// TestSecretKeysInStripsThePrefix states the two rules on their own, so a
// change to them fails with a readable message rather than only through the
// equivalence test above.
func TestSecretKeysInStripsThePrefix(t *testing.T) {
	t.Parallel()
	keys := SecretKeysIn("${SECRET_GITHUB_TOKEN} ${GITHUB_TOKEN}")
	if !slices.Equal(keys, []string{"GITHUB_TOKEN"}) {
		t.Fatalf("keys = %v, want the SECRET_-prefixed placeholder only, prefix stripped", keys)
	}
	if strings.Contains(keys[0], secretPrefix) {
		t.Errorf("key %q still carries the placeholder prefix", keys[0])
	}
}
