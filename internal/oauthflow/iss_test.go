package oauthflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidateIss(t *testing.T) {
	advertised := &AuthServerMetadata{
		Issuer: "https://as.example.com",
		AuthorizationResponseIssParameterSupported: true,
	}
	legacy := &AuthServerMetadata{Issuer: "https://as.example.com"}

	tests := []struct {
		name              string
		md                *AuthServerMetadata
		iss               string
		requireAdvertised bool
		wantErr           bool
	}{
		{name: "match", md: advertised, iss: "https://as.example.com"},
		{name: "mismatch always fails", md: legacy, iss: "https://evil.example.com", wantErr: true},
		{name: "mismatch fails in manual mode too", md: advertised, iss: "https://evil.example.com", requireAdvertised: false, wantErr: true},
		{name: "missing on advertising AS fails (loopback)", md: advertised, iss: "", requireAdvertised: true, wantErr: true},
		{name: "missing on advertising AS passes in manual mode", md: advertised, iss: "", requireAdvertised: false},
		{name: "missing on legacy AS passes", md: legacy, iss: "", requireAdvertised: true},
		{name: "nil metadata passes", md: nil, iss: "anything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIss(tt.md, tt.iss, tt.requireAdvertised)
			if tt.wantErr {
				if !errors.Is(err, ErrIssuerMismatch) {
					t.Fatalf("err = %v, want ErrIssuerMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The mix-up attack shape end to end: an AS whose callback names a
// different issuer than discovery produced. The code must never be
// redeemed.
func TestLoginLoopbackRejectsWrongIss(t *testing.T) {
	as := newFakeAS(t)
	as.issOverride = "https://evil.example.com"
	f, _ := newTestFlow(t, as)

	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Scopes: []string{"read"},
		Mode: ModeLoopback, Open: as.browserFor(t), Timeout: 10 * time.Second,
	})
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch", err)
	}
	if as.exchangeCount != 0 {
		t.Fatalf("token endpoint was hit %d times; the code must not be redeemed", as.exchangeCount)
	}
}

// An AS that advertises the iss parameter but omits it: on the loopback
// path (callback arrived intact) the absence is treated as an attack.
func TestLoginLoopbackRejectsMissingAdvertisedIss(t *testing.T) {
	as := newFakeAS(t)
	as.omitIss = true
	f, _ := newTestFlow(t, as)

	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), Scopes: []string{"read"},
		Mode: ModeLoopback, Open: as.browserFor(t), Timeout: 10 * time.Second,
	})
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch", err)
	}
	if as.exchangeCount != 0 {
		t.Fatalf("token endpoint was hit %d times; the code must not be redeemed", as.exchangeCount)
	}
}
