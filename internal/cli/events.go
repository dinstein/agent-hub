package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
)

// `events` subscribes to the daemon's SSE stream — the same stream the GUI
// consumes, so a scripted debugger and the UI observe identical facts.
//
// It is online-only by nature (docs/modules/controlplane.md matrix): the stream IS the
// daemon. Offline it exits 4 rather than printing an empty stream that
// looks like "nothing is happening".

// eventsDefaultWindow is how long a non-follow `events` collects before it
// returns. Without --follow the command is a bounded sample ("what is
// happening right now"), which is what a script or a CI check wants; with
// --follow it runs until interrupted.
const eventsDefaultWindow = 5 * time.Second

// EventRow is one daemon event.
type EventRow struct {
	Topic   string          `json:"topic"`
	Kind    string          `json:"kind"`
	Rev     uint64          `json:"rev"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Human renders one event line.
func (e EventRow) Human(w io.Writer) error {
	payload := strings.TrimSpace(string(e.Payload))
	if payload == "" || payload == "null" {
		_, err := fmt.Fprintf(w, "%s/%s rev=%d\n", e.Topic, e.Kind, e.Rev)
		return err
	}
	_, err := fmt.Fprintf(w, "%s/%s rev=%d %s\n", e.Topic, e.Kind, e.Rev, payload)
	return err
}

// EventBatch is the non-follow result: every event seen in the window.
type EventBatch struct {
	Topics []string   `json:"topics"`
	Window string     `json:"window"`
	Events []EventRow `json:"events"`
}

// Human renders the batch.
func (b EventBatch) Human(w io.Writer) error {
	if len(b.Events) == 0 {
		_, err := fmt.Fprintf(w, "no events on %s within %s\n",
			strings.Join(b.Topics, ","), b.Window)
		return err
	}
	for _, e := range b.Events {
		if err := e.Human(w); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) newEventsCmd() *cobra.Command {
	var (
		follow bool
		topics []string
		window time.Duration
	)
	cmd := &cobra.Command{
		Use:   "events [--follow] [--topics t1,t2]",
		Short: "Subscribe to the daemon event stream (same source the GUI renders)",
		Long: "Subscribe to the daemon SSE stream.\n\n" +
			"Topics: " + strings.Join(eventTopics(), ", ") + " (empty = all).\n\n" +
			"With --follow the command streams until interrupted, emitting one JSON\n" +
			"envelope per event under --json (a valid NDJSON stream). Without it the\n" +
			"command samples the stream for --window and returns one envelope.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range topics {
				if !validEventTopic(t) {
					e := Usagef("unknown topic %q", t)
					e.Hint = "known topics: " + strings.Join(eventTopics(), ", ")
					return e
				}
			}
			socket, err := a.resolver.CtlSocketPath()
			if err != nil {
				return err
			}
			if _, _, err := a.requireDaemon(cmd.Context()); err != nil {
				return err
			}
			client := api.New(socket)
			defer client.Close()

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			ch, err := client.Events.Subscribe(ctx, topics...)
			if err != nil {
				return DaemonDownf("subscribing to the event stream failed: %v", err)
			}
			if follow {
				return a.streamEvents(ctx, ch)
			}
			return a.sampleEvents(ctx, ch, topics, window)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "stream until interrupted")
	cmd.Flags().StringSliceVar(&topics, "topics", nil, "only these topics (comma separated)")
	cmd.Flags().DurationVar(&window, "window", eventsDefaultWindow, "sampling window when --follow is not given")
	return cmd
}

// streamEvents emits one envelope per event, forever. Each event is a
// complete envelope so `--json --follow` is consumable line by line.
func (a *App) streamEvents(ctx context.Context, ch <-chan api.Event) error {
	p := a.printer()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := p.Emit(eventRowOf(ev)); err != nil {
				return err
			}
		}
	}
}

// sampleEvents collects events for the window and emits one envelope.
func (a *App) sampleEvents(ctx context.Context, ch <-chan api.Event, topics []string, window time.Duration) error {
	if window <= 0 {
		window = eventsDefaultWindow
	}
	if len(topics) == 0 {
		topics = eventTopics()
	}
	batch := EventBatch{Topics: topics, Window: window.String(), Events: []EventRow{}}
	deadline := time.NewTimer(window)
	defer deadline.Stop()
collect:
	for {
		select {
		case <-ctx.Done():
			break collect
		case <-deadline.C:
			break collect
		case ev, ok := <-ch:
			if !ok {
				break collect
			}
			batch.Events = append(batch.Events, eventRowOf(ev))
		}
	}
	return a.printer().Emit(batch)
}

func eventRowOf(ev api.Event) EventRow {
	return EventRow{Topic: ev.Topic, Kind: ev.Kind, Rev: ev.Rev, Payload: ev.Payload}
}

// eventTopics is the closed topic set, mirrored from internal/ctlapi so the
// CLI rejects a typo locally instead of opening a stream that will never
// deliver anything.
func eventTopics() []string {
	return []string{
		ctlapi.TopicServers, ctlapi.TopicSessions, ctlapi.TopicSkills,
	}
}

func validEventTopic(t string) bool {
	for _, known := range eventTopics() {
		if known == t {
			return true
		}
	}
	return false
}
