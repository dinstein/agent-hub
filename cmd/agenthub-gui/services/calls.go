package services

import (
	"context"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// CallsStatus returns the effective local access-ledger policy and footprint.
func (h *Hub) CallsStatus(ctx context.Context) (api.CallsStatus, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsStatus, error) {
		return c.Calls.Status(ctx)
	})
}

// CallPage returns metadata-only call rows. Decrypted payloads are available
// exclusively through CallDetail after a user selects one row.
func (h *Hub) CallPage(
	ctx context.Context, sinceMillis int64, limit int, cursor, query, client, server, tool, outcome string,
) (api.CallPage, error) {
	return call(ctx, h, func(c *api.Client) (api.CallPage, error) {
		return c.Calls.List(ctx, api.CallFilter{
			Since: time.UnixMilli(sinceMillis), Limit: limit, Cursor: cursor, Query: query,
			Client: client, Server: server, Tool: tool, Outcome: outcome,
		})
	})
}

// CallDetail opens one lifecycle and immediately decrypts its payload previews.
// The frontend drops the returned strings when its detail drawer closes.
func (h *Hub) CallDetail(ctx context.Context, id string) (api.CallDetail, error) {
	return call(ctx, h, func(c *api.Client) (api.CallDetail, error) {
		return c.Calls.Get(ctx, id)
	})
}

func (h *Hub) CallsStats(ctx context.Context, sinceMillis int64) (api.CallsStats, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsStats, error) {
		return c.Calls.Stats(ctx, time.UnixMilli(sinceMillis))
	})
}

func (h *Hub) SetCallsEnabled(ctx context.Context, enabled bool, generation uint64) (api.CallsStatus, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsStatus, error) {
		return c.Calls.SetEnabled(ctx, enabled, generation)
	})
}

func (h *Hub) RotateCallsKey(ctx context.Context, generation uint64) (api.CallsKeyRotation, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsKeyRotation, error) {
		return c.Calls.RotateKey(ctx, generation)
	})
}

func (h *Hub) VerifyCalls(ctx context.Context) (api.CallsVerify, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsVerify, error) {
		return c.Calls.Verify(ctx)
	})
}

func (h *Hub) PruneCalls(ctx context.Context, dryRun bool) (api.CallsPrune, error) {
	return call(ctx, h, func(c *api.Client) (api.CallsPrune, error) {
		return c.Calls.Prune(ctx, dryRun)
	})
}
