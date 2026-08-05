package oauthflow

import "testing"

// TestMetadataIssuerMustMatchWhereItWasFetched is the spec's own example:
// a document served by attacker.example that declares honest.example MUST be
// rejected. Without it the whole RFC 9207 defence is circular — validateIss
// compares the response's iss against this very field, so a host that serves
// both the document and the response passes by comparing itself to itself.
func TestMetadataIssuerMustMatchWhereItWasFetched(t *testing.T) {
	tests := []struct {
		name, declared, fetchedFrom string
		wantErr                     bool
	}{
		{name: "identical", declared: "https://as.example.com", fetchedFrom: "https://as.example.com"},
		{name: "trailing slash on the document", declared: "https://as.example.com/", fetchedFrom: "https://as.example.com"},
		{name: "trailing slash on the request", declared: "https://as.example.com", fetchedFrom: "https://as.example.com/"},
		{name: "with a path", declared: "https://as.example.com/tenant1", fetchedFrom: "https://as.example.com/tenant1"},
		{name: "the spec's example", declared: "https://honest.example", fetchedFrom: "https://attacker.example", wantErr: true},
		{name: "different tenant", declared: "https://as.example.com/tenant2", fetchedFrom: "https://as.example.com/tenant1", wantErr: true},
		{name: "declares nothing", declared: "", fetchedFrom: "https://as.example.com", wantErr: true},
		// Host case is folded: DNS is case-insensitive, so it hands an
		// attacker nothing, and providers really do declare it either way.
		{name: "host case differs", declared: "https://AS.example.com", fetchedFrom: "https://as.example.com"},
		// Everything else stays a difference. A downgrade is not sloppiness,
		// a spelled-out default port names a different authority string, and
		// the path is case-sensitive because it names the tenant.
		{name: "scheme downgraded", declared: "http://as.example.com", fetchedFrom: "https://as.example.com", wantErr: true},
		{name: "default port spelled out", declared: "https://as.example.com:443", fetchedFrom: "https://as.example.com", wantErr: true},
		{name: "path case differs", declared: "https://as.example.com/Tenant1", fetchedFrom: "https://as.example.com/tenant1", wantErr: true},
		{name: "not a URL at all", declared: "honest.example", fetchedFrom: "https://as.example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := parseAbsoluteURL(tt.fetchedFrom)
			if err != nil {
				t.Fatal(err)
			}
			md := &AuthServerMetadata{Issuer: tt.declared, SourceURL: tt.fetchedFrom + "/.well-known/x"}
			got := validateIssuerMatch(md, u)
			if (got != nil) != tt.wantErr {
				t.Fatalf("validateIssuerMatch = %v, wantErr %v", got, tt.wantErr)
			}
		})
	}
}

// TestCanonicalResourceLowercasesTheHost: the canonical form MCP prescribes
// for an RFC 8707 resource indicator uses a lowercase scheme AND host.
// url.Parse folds the scheme for free and leaves the host alone, so a server
// configured with any uppercase in its hostname sent a non-canonical value
// to an authorization server that compares it literally. The path is
// case-sensitive and stays untouched.
func TestCanonicalResourceLowercasesTheHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://mcp.example.com/mcp", "https://mcp.example.com/mcp"},
		{"HTTPS://MCP.Example.com/mcp", "https://mcp.example.com/mcp"},
		{"https://MCP.Example.com:8443/MixedCasePath", "https://mcp.example.com:8443/MixedCasePath"},
		{"https://mcp.example.com/", "https://mcp.example.com"},
	}
	for _, tt := range tests {
		u, err := parseAbsoluteURL(tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if got := canonicalResource(u); got != tt.want {
			t.Errorf("canonicalResource(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
