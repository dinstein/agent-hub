// Minimal DOM helpers. Everything is built with createElement and
// textContent — never innerHTML with interpolated data: server names, tool
// names and audit details all come from downstream servers, and a proxy that
// renders untrusted strings as markup is an injection surface.

export type Attrs = Record<string, string | boolean | number | undefined>;

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Attrs = {},
  children: (Node | string | null | undefined)[] = [],
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === false) continue;
    if (k === "class") node.className = String(v);
    else if (k === "text") node.textContent = String(v);
    else if (k.startsWith("data-") || k === "href" || k === "title" || k === "type") {
      node.setAttribute(k, String(v));
    } else if (v === true) node.setAttribute(k, "");
    else node.setAttribute(k, String(v));
  }
  for (const c of children) {
    if (c === null || c === undefined) continue;
    node.append(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

/**
 * Makes `container`'s children exactly `nodes`, in that order, reusing every
 * node that is already there.
 *
 * The difference from clear-and-append is what survives: a node that is merely
 * MOVED keeps its scroll position, its open <details>, its hover and — where
 * it is not moved at all — the focus inside it. A list that rebuilds itself on
 * every asynchronous answer is how a page ends up flickering under the cursor
 * while nothing about it has actually changed.
 *
 * Callers key their nodes themselves (a map from id to node): this only puts
 * the nodes it is handed into the order it is handed them.
 */
export function reconcile(container: Element, nodes: Node[]): void {
  for (let i = 0; i < nodes.length; i++) {
    const want = nodes[i];
    const have = container.childNodes[i];
    if (have === want) continue;
    // insertBefore MOVES a node that is already in this container, which is
    // exactly what reordering needs; `have` being undefined appends.
    container.insertBefore(want, have ?? null);
  }
  while (container.childNodes.length > nodes.length) {
    const last = container.lastChild;
    if (!last) break;
    container.removeChild(last);
  }
}

export function clear(node: Element): void {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export type IconName = "search" | "play" | "scope" | "privacy" | "theme" | "window";

/** A tiny dependency-free icon set for application chrome. Centralising the
 * paths keeps weight, baseline and platform rendering consistent. */
export function icon(name: IconName, className = "ui-icon"): SVGSVGElement {
  const paths: Record<IconName, string> = {
    search: "M21 21l-4.35-4.35M19 11a8 8 0 1 1-16 0 8 8 0 0 1 16 0Z",
    play: "m8 5 11 7-11 7V5ZM4 5v14",
    scope: "M12 3 5 6v5c0 4.8 2.8 8.3 7 10 4.2-1.7 7-5.2 7-10V6l-7-3Zm-3 9 2 2 4-4",
    privacy: "M2.5 12s3.5-6 9.5-6 9.5 6 9.5 6-3.5 6-9.5 6S2.5 12 2.5 12Zm12.5 0a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z",
    theme: "M12 3a9 9 0 1 0 9 9c0-1-.2-1.9-.5-2.8A7 7 0 0 1 12 3Z",
    window: "M3 5.5A1.5 1.5 0 0 1 4.5 4h15A1.5 1.5 0 0 1 21 5.5v13a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 18.5v-13Zm0 3.5h18M6.5 6.7h.01M9 6.7h.01",
  };
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", className);
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.75");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", paths[name]);
  svg.append(path);
  return svg;
}

export function section(title: string, ...children: (Node | null)[]): HTMLElement {
  return el("section", { class: "panel" }, [
    el("h2", { text: title }),
    ...children.filter((c): c is Node => c !== null),
  ]);
}

/** The title block shared by top-level pages. Actions stay outside the title
 *  text so the hierarchy survives a narrow window and assistive technology
 *  reads one unambiguous heading. */
export function pageHeader(
  title: string,
  subtitle: string,
  ...actions: (Node | null)[]
): HTMLElement {
  const kept = actions.filter((a): a is Node => a !== null);
  return el("header", { class: "page-header" }, [
    el("div", { class: "page-heading" }, [
      el("h1", { text: title }),
      el("p", { text: subtitle }),
    ]),
    kept.length > 0 ? el("div", { class: "page-actions" }, kept) : null,
  ]);
}

export function table(headers: string[], rows: (Node | string)[][]): HTMLElement {
  const thead = el("thead", {}, [el("tr", {}, headers.map((h) => el("th", { text: h })))]);
  const tbody = el(
    "tbody",
    {},
    rows.map((r) => el("tr", {}, r.map((cell) => el("td", {}, [cell])))),
  );
  return el("div", { class: "table-wrap" }, [el("table", {}, [thead, tbody])]);
}

// ---------------------------------------------------------------------------
// The three empty states
// ---------------------------------------------------------------------------

/**
 * Which of the three "there is nothing here" situations this is
 * (docs/modules/gui.md).
 *
 *   loading — we have not been told yet. A skeleton, never a sentence: a
 *             sentence about emptiness written before the answer arrives is
 *             a claim we cannot back.
 *   failed  — we asked and could not find out. This one has to SAY it is not
 *             an empty result, because the failure mode this guards against
 *             is a governance UI rendering "no rules configured" when what
 *             actually happened is "the daemon did not answer".
 *   empty   — we asked, the answer was nothing, and the next step goes here.
 */
export type EmptyKind = "loading" | "failed" | "empty";

export interface EmptyStateOptions {
  kind: EmptyKind;
  /** One line naming the situation. */
  title: string;
  /** Why, and what happens next. */
  body?: string;
  /** Retry for `failed`; the next-step call to action for `empty`. */
  actions?: (Node | null)[];
  /** Skeleton row count for `loading`. */
  rows?: number;
}

/** The single primitive behind every "nothing to show" area. */
export function emptyState(o: EmptyStateOptions): HTMLElement {
  if (o.kind === "loading") {
    const bars = Array.from({ length: o.rows ?? 3 }, (_, i) =>
      el("i", { style: `width:${[92, 76, 84, 68, 88][i % 5]}%` }),
    );
    return el("div", { class: "empty-state es-loading", "aria-busy": "true" }, [
      el("span", { class: "es-title", text: o.title }),
      el("div", { class: "skeleton" }, bars),
    ]);
  }
  const actions = (o.actions ?? []).filter((n): n is Node => n !== null && n !== undefined);
  return el(
    "div",
    {
      class: o.kind === "failed" ? "empty-state es-failed" : "empty-state es-empty",
      role: o.kind === "failed" ? "alert" : undefined,
    },
    [
      el("span", { class: "es-title", text: o.title }),
      o.body ? el("span", { text: o.body }) : null,
      actions.length > 0 ? el("div", { class: "controls" }, actions) : null,
    ],
  );
}

/** The plain "there really is nothing" state. Kept as the short spelling
 *  because most listings need exactly this and nothing more. */
export function empty(message: string, body?: string, ...actions: (Node | null)[]): HTMLElement {
  return emptyState({ kind: "empty", title: message, body, actions });
}

/** The skeleton shown while a listing is still being fetched. */
export function loadingState(title = "Loading…", rows = 3): HTMLElement {
  return emptyState({ kind: "loading", title, rows });
}

/** Renders a failed call. Offline and not-implemented are distinct states:
 *  an empty list must never stand in for "we could not ask".
 *
 *  `extra` carries the raw text and the Copy control (see page.ts): the
 *  headline is a lossy summary and the full text must never be the thing the
 *  user has to go and find elsewhere. */
export function errorBox(message: string, hint?: string, extra?: Node | null): HTMLElement {
  return el("div", { class: "error", role: "alert" }, [
    el("strong", { text: message }),
    hint ? el("span", { class: "hint", text: hint }) : null,
    extra ?? null,
  ]);
}

// ---------------------------------------------------------------------------
// Small neutral renderers
// ---------------------------------------------------------------------------

/** Chip tones. Only counts that mean "health" may take a semantic tone;
 *  totals and metadata stay neutral (see the colour discipline in style.css). */
export type ChipTone = "neutral" | "success" | "warning" | "danger";

/**
 * One overview chip. A chip with a count of ZERO is not rendered — the caller
 * gets null and drops it (docs/modules/gui.md). "0 needs attention" is a
 * sentence nobody needs to read, and a row of zeroes buries the one number
 * that is not zero.
 */
export function chip(count: number, label: string, tone: ChipTone = "neutral"): HTMLElement | null {
  if (count === 0) return null;
  return el("span", { class: tone === "neutral" ? "chip" : `chip c-${tone}` }, [
    el("b", { text: String(count) }),
    el("span", { text: label }),
  ]);
}

/**
 * A chip that is also a filter.
 *
 * The counts along the top of a list answer "how many"; this one also answers
 * "which ones", by narrowing the list to what it counts. That is deliberately
 * the same affordance rather than a second control elsewhere: the number and
 * the way to see what it is made of belong together, and a summary that can be
 * opened is what lets a list stop rearranging itself to make the same point.
 *
 * Unlike `chip`, a zero count is still rendered while the filter is ON —
 * removing the control that turns it off would strand the user in a view they
 * cannot leave.
 */
export function chipToggle(
  count: number,
  label: string,
  tone: ChipTone,
  opts: { pressed: boolean; onToggle: () => void },
): HTMLElement | null {
  if (count === 0 && !opts.pressed) return null;
  const b = el("button", {
    class: tone === "neutral" ? "chip chip-toggle" : `chip chip-toggle c-${tone}`,
    type: "button",
    "aria-pressed": String(opts.pressed),
  }, [el("b", { text: String(count) }), el("span", { text: label })]) as HTMLButtonElement;
  b.addEventListener("click", opts.onToggle);
  return b;
}

/** A row of chips; renders nothing at all when every count was zero. */
export function chipRow(...chips: (HTMLElement | null)[]): HTMLElement | null {
  const kept = chips.filter((c): c is HTMLElement => c !== null);
  if (kept.length === 0) return null;
  return el("div", { class: "chips" }, kept);
}

// ---------------------------------------------------------------------------
// Error distillation
// ---------------------------------------------------------------------------

/**
 * Turns whatever a daemon, a spawned process or an HTTP peer produced into ONE
 * line (docs/modules/gui.md).
 *
 * The input is routinely a Go error chain, a Node stack trace or a signed URL
 * three hundred characters long; rendered verbatim it pushes every actionable
 * control off the row and still does not answer "what do I do". So this
 * recognises the handful of failures that actually recur in a proxy that
 * spawns processes and speaks HTTP, and names them the way the fix is named.
 *
 * Two rules hold this honest:
 *
 *   - It is PURE and lossy BY DESIGN, which is exactly why no caller may use
 *     it alone: the full text always travels with it (errorBox `extra`,
 *     failureBox below), one disclosure and one Copy button away.
 *   - An unrecognised error is never guessed at. It is condensed — first
 *     line, whitespace collapsed, long URLs cut to their origin — and
 *     nothing is invented about what it means.
 */
const HEADLINE_PATTERNS: { re: RegExp; title: (m: RegExpExecArray) => string }[] = [
  // Spawn failures: by far the most common way a stdio server does not start.
  { re: /\bENOENT\b/, title: () => "Command or file not found (ENOENT)" },
  { re: /\bEACCES\b|permission denied/i, title: () => "Permission denied (EACCES)" },
  {
    re: /\bEADDRINUSE\b[^\d]*((?:\d{1,3}\.){3}\d{1,3}:\d+|\[[^\]]+\]:\d+|:\d+)?/,
    title: (m) => (m[1] ? `Port already in use (${m[1]})` : "Port already in use (EADDRINUSE)"),
  },
  {
    re: /\bECONNREFUSED\b[^\d]*((?:\d{1,3}\.){3}\d{1,3}:\d+|\[[^\]]+\]:\d+)?/,
    title: (m) => (m[1] ? `Connection refused (${m[1]})` : "Connection refused (ECONNREFUSED)"),
  },
  { re: /\bECONNRESET\b/, title: () => "Connection reset by the peer (ECONNRESET)" },
  { re: /\bENOTFOUND\b|no such host/i, title: () => "Host not found (DNS)" },
  {
    re: /\bETIMEDOUT\b|context deadline exceeded|i\/o timeout|timed? ?out/i,
    title: () => "Timed out waiting for the server",
  },
  { re: /x509|certificate (signed|verify|is not)/i, title: () => "TLS certificate rejected" },
  { re: /\bEPIPE\b|broken pipe/i, title: () => "The server closed the connection (broken pipe)" },
  // Process exits: a server that started and then gave up.
  {
    re: /exit status (\d+)|exited with code (\d+)/i,
    title: (m) => `The server process exited with status ${m[1] ?? m[2]}`,
  },
  { re: /signal: (\w+)/, title: (m) => `The server process was killed (signal ${m[1]})` },
  // HTTP: the status is the whole message, the URL never is. The status
  // codes are matched on word boundaries and 5xx additionally demands an
  // explicit marker, so "timeout after 500ms" does not become "the remote
  // server failed" — an unrecognised error condensed is strictly better than
  // a recognised one that is wrong.
  { re: /\b401\b|\bunauthorized\b/i, title: () => "Authentication required (401)" },
  { re: /\b403\b|\bforbidden\b/i, title: () => "Access denied (403)" },
  { re: /\b404\b/, title: () => "Endpoint not found (404)" },
  { re: /\b429\b|too many requests|rate.?limit/i, title: () => "Rate limited (429)" },
  {
    re: /internal server error|\bHTTP\s*5\d{2}\b|\bstatus(?:\s+code)?\s*[:=]?\s*5\d{2}\b/i,
    title: () => "The remote server failed (5xx)",
  },
];

/** Longest headline we will render before condensing further. */
const HEADLINE_MAX = 96;

export function errorHeadline(raw: string | undefined | null): string {
  const text = (raw ?? "").trim();
  if (!text) return "The daemon reported a failure without a message.";
  for (const p of HEADLINE_PATTERNS) {
    const m = p.re.exec(text);
    if (m) return p.title(m);
  }
  return condenseLine(text);
}

/** First line, whitespace collapsed, long URLs reduced to their origin,
 *  truncated on a word boundary. No interpretation is added. */
function condenseLine(text: string): string {
  const first = text.split("\n").find((l) => l.trim().length > 0) ?? text;
  let out = first.replace(/\s+/g, " ").trim();
  out = out.replace(/https?:\/\/[^\s"']+/g, (url) => {
    if (url.length <= 48) return url;
    const slash = url.indexOf("/", url.indexOf("://") + 3);
    return slash > 0 ? `${url.slice(0, slash)}/…` : `${url.slice(0, 45)}…`;
  });
  if (out.length <= HEADLINE_MAX) return out;
  const cut = out.slice(0, HEADLINE_MAX);
  const space = cut.lastIndexOf(" ");
  return `${(space > HEADLINE_MAX / 2 ? cut.slice(0, space) : cut).trimEnd()}…`;
}

export function relTime(iso: string | undefined): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const secs = Math.round((Date.now() - t) / 1000);
  if (Math.abs(secs) < 60) return `${secs}s ago`;
  if (Math.abs(secs) < 3600) return `${Math.round(secs / 60)}m ago`;
  if (Math.abs(secs) < 86400) return `${Math.round(secs / 3600)}h ago`;
  return new Date(t).toLocaleString();
}

export function clockTime(iso: string | undefined): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  return Number.isNaN(t) ? iso : new Date(t).toLocaleTimeString();
}
