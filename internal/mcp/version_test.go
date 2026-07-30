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
		{name: "accept 2026-07-28", server: "2026-07-28", want: "2026-07-28"},
		{name: "exact match 2025-11-25", server: "2025-11-25", want: "2025-11-25"},
		{name: "downgrade to 2025-06-18", server: "2025-06-18", want: "2025-06-18"},
		{name: "downgrade to 2025-03-26", server: "2025-03-26", want: "2025-03-26"},
		{name: "older draft rejected", server: "2024-11-05", wantErr: true},
		{name: "2026-01-01 not in list", server: "2026-01-01", wantErr: true},
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

func TestNegotiateHighest(t *testing.T) {
	tests := []struct {
		name    string
		server  []string
		want    string
		wantErr bool
	}{
		{name: "picks 2026 over 2025", server: []string{"2025-11-25", "2026-07-28"}, want: "2026-07-28"},
		{name: "legacy only", server: []string{"2025-11-25"}, want: "2025-11-25"},
		{name: "ignores unknown newer", server: []string{"2099-01-01", "2025-06-18"}, want: "2025-06-18"},
		{name: "no overlap", server: []string{"2024-11-05", "2099-01-01"}, wantErr: true},
		{name: "empty list", server: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NegotiateHighest(tt.server)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedVersion) {
					t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
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

// TestProtocolVersionConsistency checks that ProtocolVersion appears in
// SupportedVersions and that NegotiateVersion accepts it.
// SupportedVersions[0] is the highest version this facade supports, which
// permanently exceeds ProtocolVersion: the constant names the stateful
// (≤ 2025-11-25) declaration, while 2026-07-28 travels per-request in _meta
// (see docs/mcp-2026-07-28.md §6.1).
func TestProtocolVersionConsistency(t *testing.T) {
	if len(SupportedVersions) == 0 {
		t.Fatal("SupportedVersions is empty")
	}
	found := false
	for _, v := range SupportedVersions {
		if v == ProtocolVersion {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ProtocolVersion %q not found in SupportedVersions %v", ProtocolVersion, SupportedVersions)
	}
	if _, err := NegotiateVersion(ProtocolVersion); err != nil {
		t.Fatal(err)
	}
}
