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

export function clear(node: Element): void {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export function section(title: string, ...children: (Node | null)[]): HTMLElement {
  return el("section", { class: "panel" }, [
    el("h2", { text: title }),
    ...children.filter((c): c is Node => c !== null),
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
