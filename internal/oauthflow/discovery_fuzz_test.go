package oauthflow

import "testing"

// FuzzScanAuthParam drives the WWW-Authenticate parameter scanner.
//
// The header comes from a REMOTE authorization server, and what is pulled out
// of it decides where the OAuth flow goes next: resource_metadata names the
// document that is fetched, scope names what is requested. The scanner walks
// the header by index with hand-rolled quoting and escape handling rather than
// using a library, which is exactly where an index runs past the end.
//
// Both parameter names are scanned per input so the fuzzer explores the
// key-matching path as well as the value-extraction one.
func FuzzScanAuthParam(f *testing.F) {
	for _, s := range []string{
		`Bearer resource_metadata="https://x/.well-known/oauth-protected-resource"`,
		`Bearer realm="x", scope="a b c"`,
		`Bearer resource_metadata=`,
		`Bearer resource_metadata="`,
		`Bearer resource_metadata="\`,
		`Bearer scope="a\"b"`,
		`Bearer ,,,=,,`,
		`Bearer resource_metadata=unquoted,scope=also`,
		``, `=`, `"`, `\`, `Bearer `,
	} {
		f.Add(s, "resource_metadata")
	}
	f.Fuzz(func(t *testing.T, h, name string) {
		_ = scanAuthParam(h, name)
		_ = scanAuthParam(h, paramResourceMetadataKey)
		_ = scanAuthParam(h, paramScopeKey)
	})
}
