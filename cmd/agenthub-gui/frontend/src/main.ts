// Application shell: hash router, daemon status indicator and the sidebar
// counters. Pages own their own data loading and subscriptions.
//
// The counters live here rather than on the pages they refer to because they
// are the BACKSTOP: they must be right while the user is looking at some
// other page, and no page-local dismissal may switch them off
// (docs/modules/gui.md).

import "./style.css";
import { EVT, hub, on } from "./bridge";
import { clear, loadingState } from "./dom";
import type { Page } from "./page";
import { failureBox } from "./page";
import { initTheme } from "./ui";
import { auditPage } from "./pages/audit";
import { authPage } from "./pages/auth";
import { catalogPage } from "./pages/catalog";
import { clientsPage } from "./pages/clients";
import { configPage } from "./pages/config";
import { onboardingPage, shouldAutoStart } from "./pages/onboarding";
import { playgroundPage } from "./pages/playground";
import { profilesPage } from "./pages/profiles";
import { scopePage } from "./pages/scope";
import { secretsPage } from "./pages/secrets";
import { serversPage } from "./pages/servers";
import { sessionsPage } from "./pages/sessions";
import { settingsPage } from "./pages/settings";
import { skillsPage } from "./pages/skills";
import { toolsPage } from "./pages/tools";
import { tokensPage } from "./pages/tokens";
import type { Status, TopicEvent } from "./types";

type Route =
  | "onboarding"
  | "playground"
  | "servers"
  | "catalog"
  | "profiles"
  | "scope"
  | "tools"
  | "config"
  | "secrets"
  | "sessions"
  | "skills"
  | "tokens"
  | "clients"
  | "auth"
  | "audit"
  | "settings";

const ROUTES: Record<Route, () => Page> = {
  onboarding: onboardingPage,
  playground: playgroundPage,
  servers: serversPage,
  catalog: catalogPage,
  profiles: profilesPage,
  scope: scopePage,
  tools: toolsPage,
  config: configPage,
  secrets: secretsPage,
  sessions: sessionsPage,
  skills: skillsPage,
  tokens: tokensPage,
  clients: clientsPage,
  auth: authPage,
  audit: auditPage,
  settings: settingsPage,
};

const view = document.getElementById("view") as HTMLElement;
const dot = document.getElementById("daemon-dot") as HTMLElement;
const statusText = document.getElementById("daemon-text") as HTMLElement;
const statusDetail = document.getElementById("daemon-detail") as HTMLElement;
const quarantineCount = document.getElementById("quarantine-count") as HTMLElement;

let current: Page | null = null;
let currentRoute: Route | null = null;

function routeFromHash(): Route {
  const name = window.location.hash.replace(/^#\/?/, "") as Route;
  return name in ROUTES ? name : "servers";
}

async function mount(route: Route): Promise<void> {
  if (route === currentRoute) return;
  current?.dispose?.();
  current = null;
  currentRoute = route;
  for (const link of Array.from(document.querySelectorAll<HTMLAnchorElement>("a.nav"))) {
    link.classList.toggle("active", link.dataset.route === route);
  }
  clear(view);
  const page = ROUTES[route]();
  current = page;
  try {
    await page.render(view);
  } catch (err) {
    clear(view);
    view.append(failureBox(err));
  }
}

function paintStatus(st: Status): void {
  dot.className = `dot ${st.connected ? "ok" : "bad"}`;
  statusText.textContent = st.connected ? "daemon connected" : "daemon offline";
  // The build and pid are what you need once something is wrong, not while
  // it is fine, so they sit on the quieter second line.
  statusDetail.textContent = st.connected ? `${st.version || "?"} · pid ${st.pid || "?"}` : "";
  statusText.title = st.error ?? "";
  statusDetail.title = st.socket ?? "";
}


/**
 * The quarantine counter, which the Tools page cannot switch off.
 *
 * Dismissing an alert there is scoped to that alert's content; this number is
 * scoped to reality. Without it, "Keep blocked" on a busy day would be
 * indistinguishable from "resolved", and the operator would have no standing
 * reminder that some tools are still isolated.
 */
async function refreshQuarantineBadge(): Promise<void> {
  try {
    const list = await hub.listQuarantine();
    const n = (list.entries ?? []).length;
    quarantineCount.textContent = String(n);
    quarantineCount.hidden = n === 0;
  } catch {
    quarantineCount.hidden = true;
  }
}

function refreshCounters(): Promise<void> {
  return refreshQuarantineBadge();
}

/**
 * Chooses the first view.
 *
 * A brand-new installation opens the setup guide instead of an empty Servers
 * table. The decision belongs to onboarding.ts (it is latched there, so a
 * later step that registers the first server cannot retract it); this only has
 * to avoid two things:
 *
 *   - overriding an explicit destination. A hash in the URL is a request, and
 *     a wizard that ignores it is a wizard the user cannot escape;
 *   - flashing the wrong page. Deciding takes two control-plane round trips,
 *     so the view holds still until the answer is in rather than rendering
 *     Servers and yanking it away.
 */
async function firstRoute(): Promise<void> {
  if (window.location.hash.replace(/^#\/?/, "") !== "") {
    await mount(routeFromHash());
    return;
  }
  // The decision costs two control-plane round trips, and on a dead daemon it
  // costs the dial timeout. A skeleton says "still working" for that window;
  // an empty pane would read as a broken app.
  view.append(loadingState("Starting…", 2));
  const wizard = await shouldAutoStart();
  clear(view);
  if (wizard) {
    // Assigning the hash fires hashchange, which mounts the page.
    window.location.hash = "#/onboarding";
    return;
  }
  await mount(routeFromHash());
}

function boot(): void {
  initTheme();
  window.addEventListener("hashchange", () => void mount(routeFromHash()));

  on<Status>(EVT.daemon, (st) => {
    paintStatus(st);
    void refreshCounters();
  });
  // The quarantine is registry-backed, so the servers topic is the cue that
  // something may have been isolated or released elsewhere.
  on<TopicEvent>(EVT.servers, () => void refreshQuarantineBadge());

  void firstRoute();
  hub
    .status()
    .then((st) => {
      paintStatus(st);
      return refreshCounters();
    })
    .catch(() => {
      // The Go side is not reachable at all (dev server opened outside the
      // desktop app): leave the indicator in its initial state.
      dot.className = "dot bad";
      statusText.textContent = "runtime unavailable";
    });
}

boot();
