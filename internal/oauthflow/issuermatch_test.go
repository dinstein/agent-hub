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
		// Nothing but a single trailing slash is normalized: case folding
		// and default-port elision stay differences, as the spec requires.
		{name: "host case differs", declared: "https://AS.example.com", fetchedFrom: "https://as.example.com", wantErr: true},
		{name: "default port spelled out", declared: "https://as.example.com:443", fetchedFrom: "https://as.example.com", wantErr: true},
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
