package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/accesslog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// AuditStatus is the public policy view. It deliberately carries the key ID
// but never the encryption key.
type AuditStatus struct {
	Enabled       bool   `json:"enabled"`
	Arguments     string `json:"arguments"`
	Results       string `json:"results"`
	ResultBytes   int    `json:"resultBytes"`
	Durability    string `json:"durability"`
	RetentionDays int    `json:"retentionDays"`
	MaxBytes      int64  `json:"maxBytes"`
	MinFreeBytes  int64  `json:"minFreeBytes"`
	Pressure      string `json:"pressure"`
	KeyID         string `json:"keyId,omitempty"`
}

func auditStatusOf(p registry.ResolvedAuditPolicy) AuditStatus {
	return AuditStatus{
		Enabled: p.Enabled, Arguments: "full", Results: p.ResultMode,
		ResultBytes: p.ResultBytes, Durability: p.Durability,
		RetentionDays: p.RetentionDays, MaxBytes: p.MaxBytes,
		MinFreeBytes: p.MinFreeBytes, Pressure: "block", KeyID: p.KeyID,
	}
}

// Human renders the effective policy, including defaults.
func (s AuditStatus) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"enabled: %s\narguments: %s\nresults: %s (%d bytes)\n"+
			"durability: %s\nretention: %d days\nmax size: %d bytes\n"+
			"free reserve: %d bytes\npressure: %s\nkey id: %s\n",
		boolText(s.Enabled), s.Arguments, s.Results, s.ResultBytes,
		s.Durability, s.RetentionDays, s.MaxBytes, s.MinFreeBytes,
		s.Pressure, dash(s.KeyID),
	)
	return err
}

func (a *App) newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Configure and inspect the local tools/call access ledger",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newAuditStatusCmd(), a.newAuditEnableCmd(), a.newAuditDisableCmd())
	return cmd
}

func (a *App) newAuditStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the effective capture, durability and retention policy",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			return a.printer().Emit(auditStatusOf(store.Snapshot().Governance.V.ResolvedAudit()), warnings...)
		},
	}
}

func (a *App) newAuditEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Create the payload key if needed and enable strict access recording",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			key, err := a.loadOrCreateAuditKey(cmd)
			if err != nil {
				return err
			}
			keyID, err := accesslog.KeyID(key)
			if err != nil {
				return err
			}
			for i := range key {
				key[i] = 0
			}
			res, err := confops.SetAuditEnabled(cmd.Context(), store, true, keyID, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(auditStatusOf(res.Policy), warnings...)
		},
	}
}

func (a *App) newAuditDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Stop recording new calls without deleting keys or existing history",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetAuditEnabled(cmd.Context(), store, false, "", noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(auditStatusOf(res.Policy), warnings...)
		},
	}
}

func (a *App) loadOrCreateAuditKey(cmd *cobra.Command) ([]byte, error) {
	chain, _, err := a.secretChain()
	if err != nil {
		return nil, err
	}
	ref := secrets.AuditEncryptionRef()
	encoded, ok, err := chain.Get(cmd.Context(), ref)
	if err != nil {
		return nil, classifySecretsError(err)
	}
	if !ok {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate audit encryption key: %w", err)
		}
		encoded = base64.RawStdEncoding.EncodeToString(key)
		if err := chain.Set(cmd.Context(), ref, encoded); err != nil {
			for i := range key {
				key[i] = 0
			}
			return nil, classifySecretsError(err)
		}
		return key, nil
	}
	key, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, &Error{
			Code: CodeStateCorrupt, ExitCode: ExitLocked,
			Message: "stored audit encryption key is invalid",
			Hint:    "restore the key before enabling audit; replacing it would orphan existing payloads",
			Err:     err,
		}
	}
	return key, nil
}
