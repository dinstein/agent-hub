package secrets

import "testing"

// TestStorageKeyGolden freezes the storage-key encoding. These strings are
// the on-disk / in-keyring ABI: if this test fails, stored secrets have
// been orphaned — fix the code, never the golden values.
func TestStorageKeyGolden(t *testing.T) {
	cases := []struct {
		ref  Ref
		want string
	}{
		{Ref{ServerID: "github", Key: "api_token"}, "agenthub/v1/github/_global/api_token"},
		{Ref{ServerID: "github", Scope: "work", Key: "api_token"}, "agenthub/v1/github/work/api_token"},
		{Ref{ServerID: "github", Key: KeyHTTPAuth}, "agenthub/v1/github/_global/__http_auth__"},
		{Ref{ServerID: "github", Scope: "work", Key: KeyOAuthState}, "agenthub/v1/github/work/__oauth_state__"},
		{Ref{ServerID: "my/server", Scope: "sc%ope", Key: "k/1"}, "agenthub/v1/my%2Fserver/sc%25ope/k%2F1"},
		{Ref{ServerID: "a%2Fb", Key: "k"}, "agenthub/v1/a%252Fb/_global/k"},
		{Ref{ServerID: "s", Scope: "_global", Key: "k:v.1"}, "agenthub/v1/s/_global/k:v.1"},
	}
	for _, tc := range cases {
		if got := tc.ref.StorageKey(); got != tc.want {
			t.Errorf("StorageKey(%+v) = %q, want %q", tc.ref, got, tc.want)
		}
		back, err := ParseStorageKey(tc.want)
		if err != nil {
			t.Errorf("ParseStorageKey(%q): %v", tc.want, err)
			continue
		}
		// Scope normalizes to explicit "_global" on parse.
		want := tc.ref
		if want.Scope == "" {
			want.Scope = DefaultScope
		}
		if back != want {
			t.Errorf("ParseStorageKey(%q) = %+v, want %+v", tc.want, back, want)
		}
	}
}

func TestParseStorageKeyRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"agenthub/v1/a/b",            // too few parts
		"agenthub/v1/a/b/c/d",        // too many parts
		"agenthub/v2/a/_global/k",    // unknown version
		"other/v1/a/_global/k",       // wrong prefix
		"agenthub/v1/a/_global/k%",   // truncated escape
		"agenthub/v1/a/_global/k%zz", // invalid escape
		"agenthub/v1//_global/k",     // empty server id
		"agenthub/v1/a/_global/%20",  // invalid escape (only %25/%2F defined)
		"agenthub/v1/a/_global/",     // empty key
	} {
		if _, err := ParseStorageKey(s); err == nil {
			t.Errorf("ParseStorageKey(%q): expected error", s)
		}
	}
}

func TestRefValidate(t *testing.T) {
	if err := (Ref{ServerID: "s", Key: "k"}).Validate(); err != nil {
		t.Fatalf("valid ref rejected: %v", err)
	}
	for _, r := range []Ref{
		{},
		{ServerID: "s"},
		{Key: "k"},
		{ServerID: "  ", Key: "k"},
		{ServerID: "s", Key: " \t"},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v): expected error", r)
		}
	}
}

// TestEnvNameGolden freezes the env-var name mapping (also ABI).
func TestEnvNameGolden(t *testing.T) {
	cases := []struct{ key, want string }{
		{"api_token", "AGENTHUB_SECRET_API_TOKEN"},
		{"api-token", "AGENTHUB_SECRET_API_TOKEN"},
		{"ApiToken", "AGENTHUB_SECRET_APITOKEN"},
		{"a.b c", "AGENTHUB_SECRET_A_B_C"},
		{"__http_auth__", "AGENTHUB_SECRET___HTTP_AUTH__"},
		{"key", "AGENTHUB_SECRET_KEY"}, // reserved collision, see chain test
	}
	for _, tc := range cases {
		if got := EnvName(tc.key); got != tc.want {
			t.Errorf("EnvName(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := BareEnvName("api-token"); got != "API_TOKEN" {
		t.Errorf("BareEnvName = %q, want API_TOKEN", got)
	}
}

// TestWellKnownRefsAreDistinctAndFrozen pins the two properties the well-known
// refs carry.
//
// FROZEN: these storage keys name credentials already sitting in a user's
// vault. Changing a spelling does not fail — it orphans the entry, and the
// symptom is a server that asks for a login it already has.
//
// DISTINCT: the access token, the OAuth state and a user-named secret must
// never share a slot. A collision means one silently overwrites another, and
// the OAuth state holds the refresh token — the credential that cannot be
// re-derived without a full login.
func TestWellKnownRefsAreDistinctAndFrozen(t *testing.T) {
	refs := map[string]Ref{
		"agenthub/v1/gh/_global/__http_auth__":                                HTTPAuthRef("gh"),
		"agenthub/v1/gh/_global/__oauth_state__":                              OAuthStateRef("gh"),
		"agenthub/v1/gh/_global/api_key":                                      UserRef("gh", "api_key"),
		"agenthub/v1/_agenthub/_global/__audit_encryption__":                  CallsEncryptionRef(),
		"agenthub/v1/_agenthub/_global/__audit_encryption_0123456789abcdef__": CallsEncryptionKeyRef("0123456789abcdef"),
	}
	seen := map[string]string{}
	for want, ref := range refs {
		got := ref.StorageKey()
		if got != want {
			t.Errorf("StorageKey = %q, want the frozen %q", got, want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q collides with %q: one credential would overwrite the other", got, prev)
		}
		seen[got] = want
		// Every well-known ref must survive the round trip, or a listing
		// cannot name what it found.
		back, err := ParseStorageKey(got)
		if err != nil {
			t.Errorf("ParseStorageKey(%q): %v", got, err)
			continue
		}
		if back.ServerID != ref.ServerID || back.Key != ref.Key {
			t.Errorf("round trip of %q gave %+v", got, back)
		}
	}

	// Different servers never share a slot either.
	if HTTPAuthRef("a").StorageKey() == HTTPAuthRef("b").StorageKey() {
		t.Error("two servers share one access-token slot")
	}
}

// TestOAuthStoreUsesTheWellKnownRefs is the anti-drift check.
//
// internal/oauthflow reads and writes these two entries. It used to spell the
// composite key itself, so the vault key was computed by one path in the tests
// (which called these helpers) and another in production. A change to a helper
// would have been followed by the tests and not by the code, leaving the suite
// green while the two diverged — and the divergence only shows up as a
// credential that cannot be found.
//
// The literals below are the shape oauthflow must produce. They are written
// out rather than taken from the helpers so this test still fails if BOTH
// sides are changed together.
func TestOAuthStoreUsesTheWellKnownRefs(t *testing.T) {
	if got := OAuthStateRef("srv").StorageKey(); got != "agenthub/v1/srv/_global/__oauth_state__" {
		t.Errorf("oauth state slot moved to %q", got)
	}
	if got := HTTPAuthRef("srv").StorageKey(); got != "agenthub/v1/srv/_global/__http_auth__" {
		t.Errorf("access token slot moved to %q", got)
	}
}
