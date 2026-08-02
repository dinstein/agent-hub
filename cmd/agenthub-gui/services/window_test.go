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
	// Every change is announced, whichever surface made it: the preference is
	// shown in two places (Settings and the tray checkbox) and only one of
	// them made the change. An announcement covering one direction left the
	// other stale — which is what made the Settings switch look inert.
	if evs := rec.byName(EventWindowPrefs); len(evs) != 1 {
		t.Fatalf("SetWindowPreferences emitted %d events, want 1", len(evs))
	}

	if got := h.ToggleCloseToTray(); !got.CloseToTray || !got.HideNoticeSeen {
		t.Fatalf("ToggleCloseToTray returned %+v, want close-to-tray back on with the notice intact", got)
	}
	evs := rec.byName(EventWindowPrefs)
	if len(evs) != 2 {
		t.Fatalf("the tray change emitted %d events in total, want 2", len(evs))
	}
	last := evs[len(evs)-1]
	if got, ok := last.data.(WindowPrefs); !ok || !got.CloseToTray {
		t.Fatalf("event payload = %#v", last.data)
	}
	if h.WindowPreferences() != last.data {
		t.Fatalf("the announced value %+v is not the stored one %+v", last.data, h.WindowPreferences())
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
