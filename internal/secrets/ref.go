package secrets

import (
	"fmt"
	"strings"
)

// DefaultScope is the scope component used when Ref.Scope is empty. The
// vault key is composite (ServerID, Scope) from day one (ruling A.5 #26)
// so that per-scope identities never require a storage migration.
const DefaultScope = "_global"

// Well-known vault entry keys for remote servers (docs/modules/oauth.md: two vault
// entries per remote server). This milestone ships only the Store plumbing;
// oauthflow (M1) is the writer and owns the ordering invariant "state is
// written before the access token".
const (
	// KeyHTTPAuth holds the access token (OAuth and hand-pasted tokens
	// share this slot).
	KeyHTTPAuth = "__http_auth__"
	// KeyOAuthState holds the OAuth state JSON (token_endpoint, client_id,
	// refresh_token, resource, issued_at, expires_at).
	KeyOAuthState = "__oauth_state__"
	// KeyCallsEncryption holds the machine-local ledger key.
	//
	// The stored NAME keeps its original spelling on purpose: it is what
	// every existing installation's vault entry is filed under, and renaming
	// it would lose the key that decrypts their history. It is
	// internal infrastructure, never a downstream credential.
	KeyCallsEncryption = "__audit_encryption__"
)

// Ref identifies one secret: the composite vault key (ServerID, Scope)
// plus the entry Key. An empty Scope means DefaultScope.
type Ref struct {
	ServerID string
	Scope    string
	Key      string
}

// scopeName returns the scope component with the default applied.
func (r Ref) scopeName() string {
	if r.Scope == "" {
		return DefaultScope
	}
	return r.Scope
}

// Validate rejects refs that cannot be stored. ServerID and Key must be
// non-blank; Scope may be empty (defaulted to DefaultScope).
func (r Ref) Validate() error {
	if strings.TrimSpace(r.ServerID) == "" {
		return fmt.Errorf("secrets: ref has empty server id")
	}
	if strings.TrimSpace(r.Key) == "" {
		return fmt.Errorf("secrets: ref %q has empty key", r.ServerID)
	}
	return nil
}

// storageKeyPrefix versions the storage encoding. Bumping it is a storage
// migration, never a silent change.
const storageKeyPrefix = "agenthub/v1"

// StorageKey returns the stable storage encoding of the ref, used verbatim
// as the keyring account name and as the secrets.enc map key:
//
//	agenthub/v1/<serverID>/<scope>/<key>
//
// with '%' and '/' percent-escaped inside each component so the encoding
// is injective. This encoding is FROZEN and golden-tested — changing it
// orphans every stored secret.
func (r Ref) StorageKey() string {
	return storageKeyPrefix + "/" +
		escapeComponent(r.ServerID) + "/" +
		escapeComponent(r.scopeName()) + "/" +
		escapeComponent(r.Key)
}

// ParseStorageKey inverts StorageKey. Unknown prefixes and malformed
// escapes are errors (fail-closed: a key we cannot decode is surfaced, not
// silently dropped from listings).
func ParseStorageKey(s string) (Ref, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 || parts[0] != "agenthub" || parts[1] != "v1" {
		return Ref{}, fmt.Errorf("secrets: malformed storage key %q", s)
	}
	server, err := unescapeComponent(parts[2])
	if err != nil {
		return Ref{}, err
	}
	scope, err := unescapeComponent(parts[3])
	if err != nil {
		return Ref{}, err
	}
	key, err := unescapeComponent(parts[4])
	if err != nil {
		return Ref{}, err
	}
	ref := Ref{ServerID: server, Scope: scope, Key: key}
	if err := ref.Validate(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// escapeComponent percent-escapes '%' and '/' — the only bytes that would
// break the separator structure. Everything else passes through verbatim
// so keys stay human-readable in `secret ls` output.
func escapeComponent(s string) string {
	if !strings.ContainsAny(s, "%/") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%':
			b.WriteString("%25")
		case '/':
			b.WriteString("%2F")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func unescapeComponent(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("secrets: truncated escape in %q", s)
		}
		switch s[i+1 : i+3] {
		case "25":
			b.WriteByte('%')
		case "2F":
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("secrets: invalid escape %q in %q", s[i:i+3], s)
		}
		i += 2
	}
	return b.String(), nil
}
