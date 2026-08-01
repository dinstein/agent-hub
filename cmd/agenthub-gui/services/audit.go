package services

import (
	"context"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// AuditStatus returns the effective local access-ledger policy and footprint.
func (h *Hub) AuditStatus(ctx context.Context) (api.AuditStatus, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditStatus, error) {
		return c.Audit.Status(ctx)
	})
}

// AuditCalls returns metadata-only call rows. Decrypted payloads are available
// exclusively through AuditCall after a user selects one row.
func (h *Hub) AuditCalls(
	ctx context.Context, sinceMillis int64, limit int, cursor, query, client, server, tool, outcome string,
) (api.AuditCalls, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditCalls, error) {
		return c.Audit.Calls(ctx, api.AuditCallFilter{
			Since: time.UnixMilli(sinceMillis), Limit: limit, Cursor: cursor, Query: query,
			Client: client, Server: server, Tool: tool, Outcome: outcome,
		})
	})
}

// AuditCall opens one lifecycle and immediately decrypts its payload previews.
// The frontend drops the returned strings when its detail drawer closes.
func (h *Hub) AuditCall(ctx context.Context, id string) (api.AuditCallDetail, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditCallDetail, error) {
		return c.Audit.Call(ctx, id)
	})
}

func (h *Hub) AuditStats(ctx context.Context, sinceMillis int64) (api.AuditStats, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditStats, error) {
		return c.Audit.Stats(ctx, time.UnixMilli(sinceMillis))
	})
}

func (h *Hub) SetAuditEnabled(ctx context.Context, enabled bool, generation uint64) (api.AuditStatus, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditStatus, error) {
		return c.Audit.SetEnabled(ctx, enabled, generation)
	})
}

func (h *Hub) RotateAuditKey(ctx context.Context, generation uint64) (api.AuditKeyRotation, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditKeyRotation, error) {
		return c.Audit.RotateKey(ctx, generation)
	})
}

func (h *Hub) VerifyAudit(ctx context.Context) (api.AuditVerify, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditVerify, error) {
		return c.Audit.Verify(ctx)
	})
}

func (h *Hub) PruneAudit(ctx context.Context, dryRun bool) (api.AuditPrune, error) {
	return call(ctx, h, func(c *api.Client) (api.AuditPrune, error) {
		return c.Audit.Prune(ctx, dryRun)
	})
}
