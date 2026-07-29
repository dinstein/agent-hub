package secrets

import "strings"

// Frozen environment variable names of the secrets subsystem (ABI).
const (
	// EnvSecretPrefix prefixes level-1 per-key environment variables:
	// AGENTHUB_SECRET_<KEY> with <KEY> normalized by EnvName.
	EnvSecretPrefix = "AGENTHUB_SECRET_"
	// EnvAllowBare opts in to level 2 (bare <KEY> environment lookup)
	// when set to exactly "1".
	EnvAllowBare = "AGENTHUB_ALLOW_BARE_SECRET_ENV"
	// EnvEncKey holds the key material for secrets.enc: either 64 hex
	// characters (a raw 32-byte key) or an arbitrary passphrase (hashed
	// with SHA-256). Setting it activates level 3.
	EnvEncKey = "AGENTHUB_SECRET_KEY"
	// EnvDevSecrets forces the dev fallback (writes go to secrets.enc
	// under the auto-generated dev key) when set to exactly "1"
	// (ruling A.6 #5).
	EnvDevSecrets = "AGENTHUB_DEV_SECRETS"
)

// EnvName maps a Ref.Key to its level-1 environment variable name:
// uppercase, with every byte outside [A-Z0-9_] replaced by '_'.
//
// Note the reserved collision: a key named "key" would map to
// AGENTHUB_SECRET_KEY, which is the enc-file key material variable. The
// chain never resolves that name as a secret (fail-closed: the key
// material must not be readable through the secret chain).
func EnvName(key string) string {
	return EnvSecretPrefix + normalizeEnvKey(key)
}

// BareEnvName maps a Ref.Key to its level-2 (bare) environment variable
// name — same normalization as EnvName, without the prefix.
func BareEnvName(key string) string {
	return normalizeEnvKey(key)
}

func normalizeEnvKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// envValue resolves chain levels 1 and 2 for a key. Empty or
// whitespace-only values count as unset and fall through.
func (c *Chain) envValue(key string) (string, bool) {
	if name := EnvName(key); name != EnvEncKey {
		if v, ok := c.cfg.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	if v, ok := c.cfg.LookupEnv(EnvAllowBare); ok && v == "1" {
		bare := BareEnvName(key)
		// Never resolve AGENTHUB_* internals through the bare path
		// (fail-closed: opt-in must not expose our own control variables).
		if !strings.HasPrefix(bare, "AGENTHUB_") {
			if v, ok := c.cfg.LookupEnv(bare); ok && strings.TrimSpace(v) != "" {
				return v, true
			}
		}
	}
	return "", false
}
