package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/calllog"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// CallsStatus is the public policy view. It deliberately carries the key ID
// but never the encryption key.
type CallsStatus struct {
	Enabled       bool          `json:"enabled"`
	Arguments     string        `json:"arguments"`
	Results       string        `json:"results"`
	ResultBytes   int           `json:"resultBytes"`
	Durability    string        `json:"durability"`
	RetentionDays int           `json:"retentionDays"`
	MaxBytes      int64         `json:"maxBytes"`
	MinFreeBytes  int64         `json:"minFreeBytes"`
	Pressure      string        `json:"pressure"`
	KeyID         string        `json:"keyId,omitempty"`
	Storage       calllog.Usage `json:"storage"`
}

// CallsKeyRotation reports only public key identifiers.
type CallsKeyRotation struct {
	PreviousKeyID string `json:"previousKeyId"`
	KeyID         string `json:"keyId"`
	Enabled       bool   `json:"enabled"`
}

func (r CallsKeyRotation) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "ledger key rotated: %s -> %s (enabled: %s)\n", r.PreviousKeyID, r.KeyID, boolText(r.Enabled))
	return err
}

func auditStatusOf(p registry.ResolvedCallsPolicy) CallsStatus {
	return CallsStatus{
		Enabled: p.Enabled, Arguments: "full", Results: p.ResultMode,
		ResultBytes: p.ResultBytes, Durability: p.Durability,
		RetentionDays: p.RetentionDays, MaxBytes: p.MaxBytes,
		MinFreeBytes: p.MinFreeBytes, Pressure: "block", KeyID: p.KeyID,
	}
}

func (a *App) auditStatus(p registry.ResolvedCallsPolicy) (CallsStatus, error) {
	status := auditStatusOf(p)
	root, err := calllog.DefaultDir(a.resolver)
	if err != nil {
		return CallsStatus{}, err
	}
	status.Storage, err = calllog.Inspect(root)
	return status, err
}

// Human renders the effective policy, including defaults.
func (s CallsStatus) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"enabled: %s\narguments: %s\nresults: %s (%d bytes)\n"+
			"durability: %s\nretention: %d days\nmax size: %d bytes\n"+
			"free reserve: %d bytes\npressure: %s\nkey id: %s\n",
		boolText(s.Enabled), s.Arguments, s.Results, s.ResultBytes,
		s.Durability, s.RetentionDays, s.MaxBytes, s.MinFreeBytes,
		s.Pressure, dash(s.KeyID),
	)
	if err == nil {
		_, err = fmt.Fprintf(w, "stored: %d bytes across %d day(s), %d pack(s)\n",
			s.Storage.Bytes, s.Storage.Days, s.Storage.PackFiles)
	}
	return err
}

func (a *App) newCallsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calls",
		Short: "Configure and inspect the local record of what clients called",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(
		a.newCallsStatusCmd(), a.newCallsTailCmd(), a.newCallsShowCmd(),
		a.newCallsStatsCmd(), a.newCallsVerifyCmd(), a.newCallsExportCmd(),
		a.newCallsPruneCmd(), a.newCallsRotateKeyCmd(),
		a.newCallsEnableCmd(), a.newCallsDisableCmd(),
	)
	return cmd
}

func (a *App) newCallsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the effective capture, durability and retention policy",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			status, err := a.auditStatus(store.Snapshot().Governance.V.ResolvedCalls())
			if err != nil {
				return err
			}
			return a.printer().Emit(status, warnings...)
		},
	}
}

func (a *App) newCallsEnableCmd() *cobra.Command {
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
			keyID, err := calllog.KeyID(key)
			if err != nil {
				return err
			}
			for i := range key {
				key[i] = 0
			}
			res, err := confops.SetCallsEnabled(cmd.Context(), store, true, keyID, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			status, err := a.auditStatus(res.Policy)
			if err != nil {
				return err
			}
			return a.printer().Emit(status, warnings...)
		},
	}
}

func (a *App) newCallsDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Stop recording new calls without deleting keys or existing history",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetCallsEnabled(cmd.Context(), store, false, "", noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			status, err := a.auditStatus(res.Policy)
			if err != nil {
				return err
			}
			return a.printer().Emit(status, warnings...)
		},
	}
}

func (a *App) newCallsRotateKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-key",
		Short: "Create a new payload key while retaining old keys for history",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			previous := store.Snapshot().Governance.V.ResolvedCalls()
			if previous.KeyID == "" {
				return Usagef("the ledger has no current key; run agenthub calls enable first")
			}
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return fmt.Errorf("generate audit encryption key: %w", err)
			}
			defer zeroSecret(key)
			keyID, err := calllog.KeyID(key)
			if err != nil {
				return err
			}
			encoded := base64.RawStdEncoding.EncodeToString(key)
			chain, _, err := a.secretChain()
			if err != nil {
				return err
			}
			// Persist by immutable id first. The configuration can then switch
			// atomically between two keys that are both already readable.
			if err := chain.Set(cmd.Context(), secrets.AuditEncryptionKeyRef(keyID), encoded); err != nil {
				return classifySecretsError(err)
			}
			res, err := confops.SetAuditKeyID(cmd.Context(), store, keyID, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			// Keep the legacy current-key entry updated for older clients. New
			// gateways resolve the immutable id entry above.
			if err := chain.Set(cmd.Context(), secrets.AuditEncryptionRef(), encoded); err != nil {
				return classifySecretsError(err)
			}
			return a.printer().Emit(CallsKeyRotation{
				PreviousKeyID: previous.KeyID, KeyID: keyID, Enabled: res.Policy.Enabled,
			}, warnings...)
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
		keyID, err := calllog.KeyID(key)
		if err != nil {
			zeroSecret(key)
			return nil, err
		}
		if err := chain.Set(cmd.Context(), secrets.AuditEncryptionKeyRef(keyID), encoded); err != nil {
			zeroSecret(key)
			return nil, classifySecretsError(err)
		}
		return key, nil
	}
	key, err := decodeAuditKey(encoded)
	if err != nil {
		return nil, err
	}
	keyID, err := calllog.KeyID(key)
	if err != nil {
		zeroSecret(key)
		return nil, err
	}
	// Backfill the immutable id entry for installations created by the first
	// audit release. This is idempotent and keeps rotation lossless.
	if err := chain.Set(cmd.Context(), secrets.AuditEncryptionKeyRef(keyID), encoded); err != nil {
		zeroSecret(key)
		return nil, classifySecretsError(err)
	}
	return key, nil
}

func decodeAuditKey(encoded string) ([]byte, error) {
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
