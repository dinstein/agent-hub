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
		responseIssuer    string
		wantErr           bool
	}{
		{name: "match", md: advertised, iss: "https://as.example.com"},
		{name: "mismatch always fails", md: legacy, iss: "https://evil.example.com", wantErr: true},
		{name: "mismatch fails in manual mode too", md: advertised, iss: "https://evil.example.com", requireAdvertised: false, wantErr: true},
		{name: "missing on advertising AS fails (loopback)", md: advertised, iss: "", requireAdvertised: true, wantErr: true},
		{name: "missing on advertising AS passes in manual mode", md: advertised, iss: "", requireAdvertised: false},
		{name: "missing on legacy AS passes", md: legacy, iss: "", requireAdvertised: true},
		{name: "nil metadata passes", md: nil, iss: "anything"},
		{name: "pinned upstream issuer matches", md: advertised, iss: "https://accounts.google.com", responseIssuer: "https://accounts.google.com"},
		{name: "pinned upstream issuer rejects discovered issuer", md: advertised, iss: "https://as.example.com", responseIssuer: "https://accounts.google.com", wantErr: true},
		{name: "pinned upstream issuer requires iss in manual mode", md: legacy, responseIssuer: "https://accounts.google.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIss(tt.md, tt.iss, tt.requireAdvertised, tt.responseIssuer)
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

// TestValidateResponseIssuer 校验用户显式配置的 issuer 必须是明确的 HTTPS URL。
func TestValidateResponseIssuer(t *testing.T) {
	for _, issuer := range []string{"http://accounts.google.com", "accounts.google.com", "https://accounts.google.com?x=1", "https://accounts.google.com#x", "https://user@accounts.google.com"} {
		if err := validateResponseIssuer(issuer); err == nil {
			t.Errorf("issuer %q was accepted", issuer)
		}
	}
	if err := validateResponseIssuer("https://accounts.google.com"); err != nil {
		t.Fatal(err)
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

// TestLoginLoopbackAcceptsPinnedResponseIssuer 覆盖代理授权服务将上游 issuer
// 原样带回客户端的场景；只有显式声明的 issuer 才允许进入 token exchange。
func TestLoginLoopbackAcceptsPinnedResponseIssuer(t *testing.T) {
	as := newFakeAS(t)
	as.issOverride = "https://accounts.google.com"
	f, _ := newTestFlow(t, as)

	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), ResponseIssuer: "https://accounts.google.com",
		Mode: ModeLoopback, Open: as.browserFor(t), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if as.exchangeCount != 1 {
		t.Fatalf("token endpoint was hit %d times, want 1", as.exchangeCount)
	}
}

// TestLoginLoopbackRejectsUnpinnedResponseIssuer 确保显式信任仍是精确匹配。
func TestLoginLoopbackRejectsUnpinnedResponseIssuer(t *testing.T) {
	as := newFakeAS(t)
	as.issOverride = "https://evil.example.com"
	f, _ := newTestFlow(t, as)

	_, err := f.Login(context.Background(), LoginRequest{
		ServerID: "gh", Issuer: as.issuer(), ResponseIssuer: "https://accounts.google.com",
		Mode: ModeLoopback, Open: as.browserFor(t), Timeout: 10 * time.Second,
	})
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch", err)
	}
	if as.exchangeCount != 0 {
		t.Fatalf("token endpoint was hit %d times; unexpected issuer must not be redeemed", as.exchangeCount)
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
