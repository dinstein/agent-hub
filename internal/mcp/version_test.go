package mcp

import (
	"errors"
	"testing"
)

func TestNegotiateVersion(t *testing.T) {
	tests := []struct {
		name    string
		server  string
		want    string
		wantErr bool
	}{
		{name: "exact match", server: "2025-11-25", want: "2025-11-25"},
		{name: "downgrade to 2025-06-18", server: "2025-06-18", want: "2025-06-18"},
		{name: "downgrade to 2025-03-26", server: "2025-03-26", want: "2025-03-26"},
		{name: "older draft rejected", server: "2024-11-05", wantErr: true},
		{name: "future version rejected", server: "2026-01-01", wantErr: true},
		{name: "empty rejected", server: "", wantErr: true},
		{name: "garbage rejected", server: "banana", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NegotiateVersion(tt.server)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if !errors.Is(err, ErrUnsupportedVersion) {
					t.Fatalf("error %v is not ErrUnsupportedVersion", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("negotiated %q, want %q", got, tt.want)
			}
		})
	}
}

// The declared client version must itself be acceptable, and must lead
// SupportedVersions (newest first).
func TestProtocolVersionConsistency(t *testing.T) {
	if len(SupportedVersions) == 0 || SupportedVersions[0] != ProtocolVersion {
		t.Fatalf("SupportedVersions[0] = %v, want %q first", SupportedVersions, ProtocolVersion)
	}
	if _, err := NegotiateVersion(ProtocolVersion); err != nil {
		t.Fatal(err)
	}
}
