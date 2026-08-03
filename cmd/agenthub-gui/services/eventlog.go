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
// class is "routine" or "disruption" — the hub running as intended versus the
// hub reacting to something that went wrong. It is a server-side selector for
// the reason every other one is: the read is limited, so a page filtered here
// would search only the newest records and report "nothing went wrong" for an
// outage that is merely older than the window.
func (h *Hub) EventLog(
	ctx context.Context, sinceMillis int64, limit int,
	scope, server, client, class string, kinds []string, cursor string,
) (api.EventLog, error) {
	return call(ctx, h, func(c *api.Client) (api.EventLog, error) {
		f := api.EventLogFilter{
			Limit: limit, Scope: scope, Server: server, Client: client,
			Class: class, Kinds: kinds, Cursor: cursor,
		}
		if sinceMillis > 0 {
			f.Since = time.UnixMilli(sinceMillis)
		}
		return c.Events.Log(ctx, f)
	})
}

// ProcLogs reads the hub's own process logs — daemon.log and every
// gateway-<client>.log — merged, newest first.
//
// It is the third of the three observability reads and the last to arrive
// here. The other two were already served over this link; these files were
// readable only from a terminal, which meant the GUI could not show the half
// of the record that explains a downstream failure, because the daemon never
// dials a downstream and the gateway that does writes to its own file.
//
// sinceMillis is 0 for "no lower bound"; every other selector is exact-match
// and empty means "no rule". cursor resumes a previous page.
func (h *Hub) ProcLogs(
	ctx context.Context, sinceMillis int64, limit int,
	source, level, client, server, cursor string,
) (api.ProcLogPage, error) {
	return call(ctx, h, func(c *api.Client) (api.ProcLogPage, error) {
		f := api.ProcLogFilter{
			Limit: limit, Source: source, Level: level,
			Client: client, Server: server, Cursor: cursor,
		}
		if sinceMillis > 0 {
			f.Since = time.UnixMilli(sinceMillis)
		}
		return c.Logs.List(ctx, f)
	})
}
