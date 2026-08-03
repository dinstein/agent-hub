package services

import (
	"context"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// EventLog reads the control-plane event log — what happened to a downstream
// server, a gateway or the daemon, in a closed vocabulary.
//
// It is not the SSE stream this package also subscribes to. That one says
// "something about servers changed just now" and carries a snapshot; this
// one says what MOVED, and is what a timeline renders. The frontend uses
// both: the stream to know when to re-read, this to know what to draw.
//
// sinceMillis is 0 for "no lower bound". Every other selector is exact-match
// and empty means "no rule", never "match nothing".
func (h *Hub) EventLog(
	ctx context.Context, sinceMillis int64, limit int, scope, server, client string, kinds []string,
) (api.EventLog, error) {
	return call(ctx, h, func(c *api.Client) (api.EventLog, error) {
		f := api.EventLogFilter{
			Limit: limit, Scope: scope, Server: server, Client: client, Kinds: kinds,
		}
		if sinceMillis > 0 {
			f.Since = time.UnixMilli(sinceMillis)
		}
		return c.Events.Log(ctx, f)
	})
}
