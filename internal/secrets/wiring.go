package secrets

// This file is wiring only: the well-known refs the HTTP downstream
// connector, the OAuth flow and the CLI all need to name. They live here so
// the composite-key shape (ServerID, Scope, Key) is spelled in exactly one
// place — a caller that builds a Ref literal is one refactor away from
// forgetting the scope component and silently reading a different entry.

// HTTPAuthRef is the access-token entry of one server at the default scope.
// OAuth-minted and hand-pasted tokens share this slot on purpose: the
// downstream connector reads ONE key regardless of how the credential was
// obtained (docs/modules/oauth.md).
func HTTPAuthRef(serverID string) Ref {
	return Ref{ServerID: serverID, Scope: DefaultScope, Key: KeyHTTPAuth}
}

// OAuthStateRef is the OAuth state entry (token endpoint, client
// credentials, refresh token, expiry) of one server at the default scope.
func OAuthStateRef(serverID string) Ref {
	return Ref{ServerID: serverID, Scope: DefaultScope, Key: KeyOAuthState}
}

// UserRef is a user-named secret of one server at the default scope — the
// target of a ${SECRET_X} placeholder in a server entry's env or headers.
func UserRef(serverID, key string) Ref {
	return Ref{ServerID: serverID, Scope: DefaultScope, Key: key}
}

// AuditEncryptionRef is the single machine-local access-ledger key. The
// reserved server component keeps it out of every downstream namespace.
func AuditEncryptionRef() Ref {
	return Ref{ServerID: "_agenthub", Scope: DefaultScope, Key: KeyAuditEncryption}
}
