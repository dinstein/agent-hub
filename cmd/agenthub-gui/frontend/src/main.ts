// Application shell: hash router, daemon status indicator and the sidebar
// counters. Pages own their own data loading and subscriptions.
//
// The counters live here rather than on the pages they refer to because they
// are the BACKSTOP: they must be right while the user is looking at some
// other page, and no page-local dismissal may switch them off
// (docs/modules/gui.md).

import "./style.css";
import { EVT, hub, on } from "./bridge";
import { clear, el, loadingState } from "./dom";
import type { Page } from "./page";
import { failureBox } from "./page";
import { initTheme } from "./ui";
import { authPage } from "./pages/auth";
import { activityPage } from "./pages/activity";
import { catalogPage } from "./pages/catalog";
import { clientsPage } from "./pages/clients";
import { configPage } from "./pages/config";
import { onboardingPage, shouldAutoStart } from "./pages/onboarding";
import { playgroundPage } from "./pages/playground";
import { profilesPage } from "./pages/profiles";
import { scopePage } from "./pages/scope";
import { serversPage } from "./pages/servers";
import { sessionsPage } from "./pages/sessions";
import { settingsPage } from "./pages/settings";
import { skillsPage } from "./pages/skills";
import { tokensPage } from "./pages/tokens";
import type { Status } from "./types";

type Route =
  | "onboarding"
  | "activity"
  | "playground"
  | "servers"
  | "catalog"
  | "profiles"
  | "scope"
  | "config"
  | "sessions"
  | "skills"
  | "tokens"
  | "clients"
  | "auth"
  | "settings";

const ROUTES: Record<Route, () => Page> = {
  activity: activityPage,
  onboarding: onboardingPage,
  playground: playgroundPage,
  servers: serversPage,
  catalog: catalogPage,
  profiles: profilesPage,
  scope: scopePage,
  config: configPage,
  sessions: sessionsPage,
  skills: skillsPage,
  tokens: tokensPage,
  clients: clientsPage,
  auth: authPage,
  settings: settingsPage,
};

const view = document.getElementById("view") as HTMLElement;
const dot = document.getElementById("daemon-dot") as HTMLElement;
const statusText = document.getElementById("daemon-text") as HTMLElement;
const statusDetail = document.getElementById("daemon-detail") as HTMLElement;
const connectionBanner = document.getElementById("connection-banner") as HTMLElement;
const appVersion = document.getElementById("app-version") as HTMLElement;

let current: Page | null = null;
let currentRoute: Route | null = null;
let mountEpoch = 0;

function routeFromHash(): Route {
  const name = window.location.hash.replace(/^#\/?/, "") as Route;
  return name in ROUTES ? name : "servers";
}

async function mount(route: Route): Promise<void> {
  if (route === currentRoute && current !== null) return;
  const epoch = ++mountEpoch;
  current?.dispose?.();
  current = null;
  currentRoute = route;
  for (const link of Array.from(document.querySelectorAll<HTMLAnchorElement>("a.nav"))) {
    link.classList.toggle("active", link.dataset.route === route);
  }
  clear(view);
  // Every page owns a detached-able host. A slow answer from the page being
  // left may still finish after dispose(); keeping its root out of the shared
  // view makes that late DOM write invisible by construction instead of
  // relying on every page to remember a post-await guard.
  const host = el("div", { class: "page-mount" });
  view.append(host);
  const page = ROUTES[route]();
  current = page;
  try {
    await page.render(host);
  } catch (err) {
    // A rejected render from an old route must not erase the page that
    // replaced it. The host check also covers a mount detached by any future
    // navigation mechanism that does not increment this counter.
    if (epoch !== mountEpoch || current !== page || !host.isConnected) return;
    clear(host);
    host.append(failureBox(err));
  }
}

function paintStatus(st: Status): void {
  dot.className = `dot ${st.connected ? "ok" : "bad"}`;
  statusText.textContent = st.connected ? "daemon connected" : "daemon offline";
  connectionBanner.hidden = st.connected;
  // The build and pid are what you need once something is wrong, not while
  // it is fine, so they sit on the quieter second line.
  statusDetail.textContent = st.connected ? `${st.version || "?"} · pid ${st.pid || "?"}` : "";
  statusText.title = st.error ?? "";
  statusDetail.title = st.socket ?? "";
}



function refreshCounters(): Promise<void> {
  return Promise.resolve();
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

  hub
    .applicationVersion()
    .then((version) => {
      appVersion.textContent = version;
    })
    .catch(() => {
      // A browser-only preview has no bound Go service. Leaving the second
      // line blank is more honest than showing a frontend package version,
      // which is deliberately unrelated to the shipped application build.
    });

  on<Status>(EVT.daemon, (st) => {
    paintStatus(st);
    void refreshCounters();
  });
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
      connectionBanner.hidden = false;
    });
}

boot();
