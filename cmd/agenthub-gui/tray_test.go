package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/cmd/agenthub-gui/services"
)

// srv builds one server with the Health fields the tray reads. Everything the
// tray decides comes from that contract, so the helper takes nothing else.
func srv(id, level, admin, state string) api.Server {
	return api.Server{
		ID:      id,
		State:   state,
		Enabled: admin != api.AdminStateDisabled,
		Health: api.Health{
			Level:      level,
			AdminState: admin,
			Summary:    level + " " + id,
		},
	}
}

func connected(servers ...api.Server) trayState {
	return trayState{
		Status:       services.Status{Connected: true, Socket: "/run/ctl.sock", Version: "0.19.3", Pid: 4213, Generation: 42},
		Servers:      servers,
		ServersKnown: true,
	}
}

func TestTrayIconFollowsHealth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state trayState
		want  trayIcon
	}{
		{"offline", trayState{}, trayIconOffline},
		{
			// Connected with nothing read yet must not claim attention.
			"connected, list not read",
			trayState{Status: services.Status{Connected: true}},
			trayIconOK,
		},
		{"all healthy", connected(srv("a", api.HealthLevelHealthy, api.AdminStateEnabled, "connected")), trayIconOK},
		{
			// Disabled is switched off on purpose, not broken: it reports
			// level=healthy by contract and must never raise the icon.
			"disabled only",
			connected(srv("a", api.HealthLevelHealthy, api.AdminStateDisabled, "stopped")),
			trayIconOK,
		},
		{"one degraded", connected(
			srv("a", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
			srv("b", api.HealthLevelDegraded, api.AdminStateEnabled, "connected"),
		), trayIconAttention},
		{"one unhealthy", connected(srv("b", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error")), trayIconAttention},
		{
			// An unreachable daemon outranks whatever the last read said.
			"stale servers while offline",
			trayState{Servers: []api.Server{srv("a", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error")}, ServersKnown: true},
			trayIconOffline,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := trayIconFor(tc.state); got != tc.want {
				t.Fatalf("trayIconFor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrayStatusLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state trayState
		want  string
	}{
		{"offline", trayState{}, "Hub offline"},
		{"connected, list not read", trayState{Status: services.Status{Connected: true}}, "Hub connected"},
		{"no servers", connected(), "Hub connected · no servers"},
		{"all active", connected(
			srv("a", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
			srv("b", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
		), "Hub connected · 2/2 servers"},
		{"some attention", connected(
			srv("a", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
			srv("b", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error"),
			srv("c", api.HealthLevelHealthy, api.AdminStateDisabled, "stopped"),
		), "Hub connected · 1/3 servers · 1 need attention"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := trayStatusLine(tc.state); got != tc.want {
				t.Fatalf("trayStatusLine = %q, want %q", got, tc.want)
			}
			if tip := trayTooltip(tc.state); !strings.HasPrefix(tip, "AgentHub — ") || !strings.HasSuffix(tip, tc.want) {
				t.Fatalf("trayTooltip = %q, want the app name in front of %q", tip, tc.want)
			}
		})
	}
}

func TestTrayServersSortWorstFirst(t *testing.T) {
	t.Parallel()
	in := []api.Server{
		srv("zeta", api.HealthLevelHealthy, api.AdminStateDisabled, "stopped"),
		srv("alpha", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
		srv("beta", api.HealthLevelDegraded, api.AdminStateEnabled, "connected"),
		srv("gamma", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error"),
		srv("delta", api.HealthLevelHealthy, api.AdminStateEnabled, "connecting"),
	}
	before := slices.Clone(in)

	got := sortServersForTray(in)

	want := []string{"gamma", "beta", "alpha", "delta", "zeta"}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d = %q, want %q (order: %v)", i, got[i].ID, id, ids(got))
		}
	}
	// The snapshot belongs to whoever else is holding it.
	for i := range before {
		if in[i].ID != before[i].ID {
			t.Fatalf("sortServersForTray reordered the caller's slice: %v", ids(in))
		}
	}
}

func ids(servers []api.Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.ID)
	}
	return out
}

func TestTrayServerItems(t *testing.T) {
	t.Parallel()

	t.Run("offline says so instead of showing an empty list", func(t *testing.T) {
		t.Parallel()
		items := trayServerItems(trayState{})
		if len(items) != 1 || items[0].Label != "Hub offline" || !items[0].Disabled {
			t.Fatalf("offline items = %+v", items)
		}
	})

	t.Run("not read yet is not the same as none", func(t *testing.T) {
		t.Parallel()
		items := trayServerItems(trayState{Status: services.Status{Connected: true}})
		if len(items) != 1 || items[0].Label != "Loading…" {
			t.Fatalf("unknown items = %+v", items)
		}
	})

	t.Run("empty offers the way to add one", func(t *testing.T) {
		t.Parallel()
		items := trayServerItems(connected())
		last := items[len(items)-1]
		if last.Action != trayActionOpen || last.Arg != "catalog" {
			t.Fatalf("empty items last = %+v", last)
		}
	})

	t.Run("rows are readouts, never actions", func(t *testing.T) {
		t.Parallel()
		items := trayServerItems(connected(
			srv("a", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error"),
			srv("b", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"),
		))
		for _, it := range items {
			if it.Separator || it.Action == trayActionOpen {
				continue
			}
			if it.Action != trayActionNone || !it.Disabled {
				t.Fatalf("server row is clickable: %+v", it)
			}
		}
		if got := items[0].Tooltip; got != "unhealthy a" {
			t.Fatalf("tooltip = %q, want the daemon's own summary", got)
		}
		if !strings.HasSuffix(items[0].Label, " a") || !strings.HasPrefix(items[0].Label, "⚠") {
			t.Fatalf("worst row = %q, want the attention glyph in front of the id", items[0].Label)
		}
	})

	t.Run("the cap says what it dropped", func(t *testing.T) {
		t.Parallel()
		var servers []api.Server
		// One broken server, then more healthy ones than the cap allows.
		servers = append(servers, srv("broken", api.HealthLevelUnhealthy, api.AdminStateEnabled, "error"))
		for i := range maxTrayServers + 4 {
			servers = append(servers, srv(string(rune('a'+i)), api.HealthLevelHealthy, api.AdminStateEnabled, "connected"))
		}
		items := trayServerItems(connected(servers...))

		if items[0].Label != "⚠ broken" {
			t.Fatalf("the cap dropped the server that mattered: first row = %q", items[0].Label)
		}
		var labels []string
		for _, it := range items {
			labels = append(labels, it.Label)
		}
		if !slices.Contains(labels, "+5 more") {
			t.Fatalf("truncation was silent: %v", labels)
		}
	})
}

func TestTrayHubItems(t *testing.T) {
	t.Parallel()

	t.Run("offline offers the one safe lifecycle action", func(t *testing.T) {
		t.Parallel()
		items := trayHubItems(trayState{Status: services.Status{Error: "dial: no such file"}})
		if items[0].Action != trayActionStartHub {
			t.Fatalf("first item = %+v, want start", items[0])
		}
		if items[len(items)-1].Tooltip != "dial: no such file" {
			t.Fatalf("last error not carried: %+v", items[len(items)-1])
		}
	})

	t.Run("connected never offers a stop or a restart", func(t *testing.T) {
		t.Parallel()
		items := trayHubItems(connected())
		for _, it := range items {
			label := strings.ToLower(it.Label)
			if strings.Contains(label, "stop") || strings.Contains(label, "restart") {
				t.Fatalf("destructive item in the tray: %+v", it)
			}
		}
		if items[0].Label != "Hub 0.19.3 · pid 4213" {
			t.Fatalf("identity row = %q", items[0].Label)
		}
	})

	t.Run("copy is disabled without a socket", func(t *testing.T) {
		t.Parallel()
		st := connected()
		st.Status.Socket = ""
		for _, it := range trayHubItems(st) {
			if it.Action == trayActionCopySocket && !it.Disabled {
				t.Fatalf("copy offered with nothing to copy: %+v", it)
			}
		}
	})
}

func TestTrayQuitLabelNamesTheConsequence(t *testing.T) {
	t.Parallel()
	if got := trayQuitLabel(trayState{}); got != "Quit AgentHub" {
		t.Fatalf("borrowed daemon quit label = %q", got)
	}
	if got := trayQuitLabel(trayState{OwnsDaemon: true}); !strings.Contains(got, "stops the hub") {
		t.Fatalf("owned daemon quit label = %q, want the consequence spelled out", got)
	}
}

func TestTrayMenuShape(t *testing.T) {
	t.Parallel()
	st := connected(srv("a", api.HealthLevelHealthy, api.AdminStateEnabled, "connected"))
	st.CloseToTray = true
	items := trayMenu(st)

	if items[0].Label != trayStatusLine(st) || !items[0].Disabled {
		t.Fatalf("first row = %+v, want the status readout", items[0])
	}
	var (
		sawOpen, sawQuit, sawCheckbox bool
		routes                        []string
	)
	for _, it := range items {
		switch {
		case it.Action == trayActionOpen && it.Arg == "":
			sawOpen = true
		case it.Action == trayActionQuit:
			sawQuit = true
		case it.Action == trayActionCloseToTray:
			sawCheckbox = it.Checkbox && it.Checked
		case it.Label == "Go to":
			for _, sub := range it.Items {
				routes = append(routes, sub.Arg)
			}
		}
	}
	if !sawOpen || !sawQuit || !sawCheckbox {
		t.Fatalf("menu is missing open/quit/preference: %+v", items)
	}
	if items[len(items)-1].Action != trayActionQuit {
		t.Fatalf("quit is not last: %+v", items[len(items)-1])
	}
	for _, r := range trayRoutes {
		if !slices.Contains(routes, r.Route) {
			t.Fatalf("route %q missing from the Go to submenu: %v", r.Route, routes)
		}
	}
}

func TestCloseIntent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                            string
		trayReady, closeToTray, noticed bool
		want                            closeIntent
	}{
		// The failure direction that matters: without a tray icon there is no
		// way back to a hidden window, so the close button keeps quitting.
		{"no tray, preference on", false, true, true, closeIntentQuit},
		{"no tray, preference off", false, false, false, closeIntentQuit},
		{"tray, preference off", true, false, true, closeIntentQuit},
		{"tray, first time", true, true, false, closeIntentAsk},
		{"tray, acknowledged", true, true, true, closeIntentHide},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := closeIntentFor(tc.trayReady, tc.closeToTray, tc.noticed); got != tc.want {
				t.Fatalf("closeIntentFor(%v, %v, %v) = %v, want %v",
					tc.trayReady, tc.closeToTray, tc.noticed, got, tc.want)
			}
		})
	}
}
