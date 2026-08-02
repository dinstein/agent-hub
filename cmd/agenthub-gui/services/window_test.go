package services

import (
	"errors"
	"testing"
)

func TestWindowPreferenceDefaults(t *testing.T) {
	// A Hub that has never heard from a frontend is the state the FIRST
	// close button press happens in, so the zero value has to be the right
	// answer rather than something NewHub patches up afterwards.
	var h Hub
	got := h.WindowPreferences()
	if !got.CloseToTray {
		t.Error("close-to-tray defaults off; the tray would then never be used")
	}
	if got.HideNoticeSeen {
		t.Error("the notice defaults to seen; a first hide would be silent")
	}
}

func TestWindowPreferenceRoundTrip(t *testing.T) {
	rec := &recorder{}
	h := &Hub{emitter: rec}

	got := h.SetWindowPreferences(WindowPrefs{CloseToTray: false, HideNoticeSeen: true})
	if got != (WindowPrefs{HideNoticeSeen: true}) {
		t.Fatalf("SetWindowPreferences returned %+v", got)
	}
	if h.WindowPreferences() != got {
		t.Fatalf("stored %+v, read back %+v", got, h.WindowPreferences())
	}
	// The frontend pushing its own stored value back is not news: it already
	// knows, and echoing it would be a write loop waiting to close.
	if evs := rec.byName(EventWindowPrefs); len(evs) != 0 {
		t.Fatalf("SetWindowPreferences emitted %d events", len(evs))
	}

	// The tray toggling it IS news: the frontend owns the durable copy.
	if got := h.ToggleCloseToTray(); !got.CloseToTray || !got.HideNoticeSeen {
		t.Fatalf("ToggleCloseToTray returned %+v, want close-to-tray back on with the notice intact", got)
	}
	evs := rec.byName(EventWindowPrefs)
	if len(evs) != 1 {
		t.Fatalf("tray change emitted %d events, want 1", len(evs))
	}
	if got, ok := evs[0].data.(WindowPrefs); !ok || !got.CloseToTray {
		t.Fatalf("event payload = %#v", evs[0].data)
	}
	if h.WindowPreferences() != evs[0].data {
		t.Fatalf("the announced value %+v is not the stored one %+v", evs[0].data, h.WindowPreferences())
	}
}

func TestOwnsDaemonFollowsTheLiveConnection(t *testing.T) {
	// The Quit label is derived from this, and the claim has to describe the
	// daemon the GUI is talking to right now — not one it started earlier.
	d := newFakeDaemon(t, pingMux(t))
	dl := &testDialer{socket: d.socket}
	dl.setSpawns(true)
	h := &Hub{dialer: dl, emitter: &recorder{}}
	t.Cleanup(h.stop)

	if h.OwnsDaemon() {
		t.Fatal("a Hub that has connected to nothing claims ownership")
	}
	if _, err := h.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !h.OwnsDaemon() {
		t.Fatal("a daemon this Hub started is not claimed")
	}

	// A transport failure drops the client, and the claim dies with it.
	h.dropClient(errors.New("write: broken pipe"))
	if h.OwnsDaemon() {
		t.Fatal("the ownership claim outlived the connection that carried it")
	}
}
