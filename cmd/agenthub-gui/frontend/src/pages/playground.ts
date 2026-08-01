// Playground: pick a server, see what it actually offers, call one tool for
// real (docs/modules/gui.md).
//
// WHY THIS PAGE EXISTS. It is not a toy. When a tool does not work inside
// Cursor or Claude Code, the operator has exactly two hypotheses — the client
// is wired wrong, or the server itself is broken — and no way to tell them
// apart from inside the client. This page answers the second one directly:
// it dials the downstream server through the daemon and calls the tool with
// arguments the operator chose. A red result here means the server; a green
// result here with a broken client means the wiring.
//
// WHAT IT IS NOT. POST /v1/servers/{id}/test is the daemon's self-test, not
// the gateway. It dials the downstream server directly and does NOT run
// pipeline.Execute — no scope resolution, no tool kill switch, no HITL gate.
// That is the right shape for a diagnostic (you must be able to probe a tool
// that governance currently blocks, or you cannot debug governance), but it
// means a call made here is not evidence that the same call would be ALLOWED
// through the gateway. The page says so on screen rather than in this comment
// only, because the person who needs to know is the one clicking the button.
//
// SCHEMAS. The tool definitions — compact signature, description and the raw
// inputSchema — come back from the same handshake that lists the names, and
// only when the request asks for them (`defs`). Nothing here re-encodes a
// schema: what the form is generated from is the downstream's own bytes, so a
// form that cannot express a parameter is evidence about the schema and not
// about our transcription of it.

import { CANCEL_CAVEAT, asCallError, hub, isCancelled } from "../bridge";
import type { Cancellable } from "../bridge";
import { clear, el, emptyState, icon, loadingState, pageHeader } from "../dom";
import type { Page } from "../page";
import { failureBox, failureState, noticeSlot } from "../page";
import {
  button,
  copyButton,
  controls,
  field,
  linesEditor,
  rawDetails,
  selectInput,
  shellArg,
  textInput,
} from "../ui";
import type {
  CallError,
  Server,
  ServerTestResult,
  ServerTestTool,
} from "../types";

// ---------------------------------------------------------------------------
// Equivalent CLI commands (docs/modules/gui.md)
// ---------------------------------------------------------------------------
//
// Both commands exist in internal/cli (server test). Nothing is invented.

const cliList = (id: string): string => `agenthub server test ${shellArg(id)} --tools`;

const cliCall = (id: string, tool: string, args: string): string =>
  `agenthub server test ${shellArg(id)} --tool ${shellArg(tool)}` +
  (args && args !== "{}" ? ` --args ${shellArg(args)}` : "");

/** How long a call may run before the daemon gives up, in milliseconds.
 *  Generous on purpose: a cold `npx` cache genuinely takes a minute, and a
 *  timeout that fires before the thing under test has finished starting
 *  produces a failure report about the wrong subject. */
const CALL_TIMEOUT_MS = 120_000;

// ---------------------------------------------------------------------------
// JSON Schema -> form
// ---------------------------------------------------------------------------

/** The slice of JSON Schema this form generator reads. Everything else in the
 *  document is preserved by being ignored: an unread keyword makes a field
 *  DEGRADE to raw JSON, it never makes the form quietly drop it. */
interface JsonSchema {
  type?: unknown;
  properties?: Record<string, unknown>;
  required?: unknown;
  enum?: unknown[];
  items?: unknown;
  description?: string;
  title?: string;
  default?: unknown;
  format?: string;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** The declared type, normalised. A union type ("string" | "null") is read as
 *  its first non-null member, which is the shape optional parameters
 *  overwhelmingly take; anything genuinely ambiguous falls through to raw
 *  JSON rather than being guessed at. */
function schemaType(s: JsonSchema): string {
  const t = s.type;
  if (typeof t === "string") return t;
  if (Array.isArray(t)) {
    const first = t.find((x) => typeof x === "string" && x !== "null");
    if (typeof first === "string") return first;
  }
  return "";
}

type SchemaCheck =
  | { ok: true; schema: JsonSchema; properties: [string, JsonSchema][]; required: Set<string> }
  | { ok: false; reason: string };

/**
 * Decides whether a form can be generated at all.
 *
 * The failure cases are named individually because they call for different
 * reactions: "no schema at all" means the server told us nothing and the
 * operator has to know what to type; "not an object schema" means the tool
 * takes something a key/value form cannot represent. Both land in raw JSON
 * mode, but only one of them is a surprise.
 */
function readObjectSchema(raw: unknown): SchemaCheck {
  if (raw === undefined || raw === null || raw === "") {
    return {
      ok: false,
      reason:
        "This server sent no input schema for the tool, so there is nothing to build a form " +
        "from. Its absence is a fact about the server, not something the GUI dropped.",
    };
  }
  if (!isRecord(raw)) {
    return { ok: false, reason: "The tool's input schema is not a JSON object." };
  }
  const s = raw as JsonSchema;
  const t = schemaType(s);
  if (t !== "" && t !== "object") {
    return {
      ok: false,
      reason: `The tool's arguments are declared as ${t}, not an object — a field-by-field form cannot represent that.`,
    };
  }
  const props = isRecord(s.properties) ? s.properties : {};
  const required = new Set<string>(
    Array.isArray(s.required) ? s.required.filter((r): r is string => typeof r === "string") : [],
  );
  const properties: [string, JsonSchema][] = Object.entries(props).map(([k, v]) => [
    k,
    isRecord(v) ? (v as JsonSchema) : {},
  ]);
  return { ok: true, schema: s, properties, required };
}

/** Why a control could not produce a value.
 *
 *  `missing` is separated from `invalid` because the two are treated
 *  differently when leaving the form for raw JSON: a required field the user
 *  has not filled in yet is not an obstacle to handing them the raw object,
 *  while a field whose contents do not parse is. Classifying that by matching
 *  the user-facing sentence would make a reworded message change behaviour. */
type FieldFault = "missing" | "invalid";

type FieldRead =
  | { ok: true; value: unknown | undefined }
  | { ok: false; fault: FieldFault; message: string };

/** One generated control. `read` returns the value to send, `undefined` for
 *  "leave this argument out entirely". */
interface FieldControl {
  node: HTMLElement;
  /** The value, or the reason it cannot be produced. */
  read(): FieldRead;
  /** Fills the control from a value that came back from raw-JSON mode. */
  write(value: unknown): void;
  /** True when this control is a raw-JSON escape hatch rather than a real
   *  rendering of the declared type. */
  degraded: boolean;
}

/** Renders an enum as a picker. `(not set)` is offered only for optional
 *  parameters: an optional argument left alone must be OMITTED, and a select
 *  with no empty option would silently send the first member instead. */
function enumControl(name: string, s: JsonSchema, required: boolean): FieldControl {
  const members = (s.enum ?? []).map((v) => ({ raw: v, text: jsonScalarText(v) }));
  const options = [
    ...(required ? [] : [{ value: "", label: "(not set)" }]),
    ...members.map((m) => ({ value: m.text, label: m.text })),
  ];
  const node = selectInput(options, required ? (members[0]?.text ?? "") : "");
  return {
    node,
    degraded: false,
    read() {
      const picked = node.value;
      if (picked === "") {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and nothing is selected.` }
          : { ok: true, value: undefined };
      }
      const hit = members.find((m) => m.text === picked);
      return { ok: true, value: hit ? hit.raw : picked };
    },
    write(value) {
      node.value = value === undefined ? "" : jsonScalarText(value);
    },
  };
}

function jsonScalarText(v: unknown): string {
  return typeof v === "string" ? v : JSON.stringify(v) ?? "";
}

function stringControl(name: string, s: JsonSchema, required: boolean): FieldControl {
  const node = textInput(
    typeof s.default === "string" ? s.default : "",
    s.format ? `${s.format}…` : "",
  );
  return {
    node,
    degraded: false,
    read() {
      const v = node.value;
      // An EMPTY optional string is omitted rather than sent as "". The two
      // are different arguments to most tools, and a form that cannot express
      // "absent" would make half of them impossible to call correctly. A
      // deliberate empty string is typed in raw JSON mode, which is exactly
      // what the mode is for.
      if (v === "") {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and empty.` }
          : { ok: true, value: undefined };
      }
      return { ok: true, value: v };
    },
    write(value) {
      node.value = value === undefined || value === null ? "" : jsonScalarText(value);
    },
  };
}

function numberControl(name: string, s: JsonSchema, required: boolean, integer: boolean): FieldControl {
  const node = textInput(typeof s.default === "number" ? String(s.default) : "", integer ? "integer" : "number");
  node.inputMode = "decimal";
  return {
    node,
    degraded: false,
    read() {
      const text = node.value.trim();
      if (text === "") {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and empty.` }
          : { ok: true, value: undefined };
      }
      const n = Number(text);
      if (!Number.isFinite(n)) {
        return { ok: false, fault: "invalid", message: `“${name}” is not a number: ${text}` };
      }
      if (integer && !Number.isInteger(n)) {
        return { ok: false, fault: "invalid", message: `“${name}” must be a whole number, got ${text}` };
      }
      return { ok: true, value: n };
    },
    write(value) {
      node.value = typeof value === "number" ? String(value) : value === undefined ? "" : jsonScalarText(value);
    },
  };
}

/**
 * A boolean as a three-state picker rather than a checkbox.
 *
 * A checkbox has two states and the argument has three: true, false, and not
 * sent. Collapsing "unticked" into `false` is the same mistake as collapsing
 * an absent selector into an empty one — for any tool whose default is `true`,
 * a form that always sends `false` silently inverts the caller's intent.
 */
function booleanControl(name: string, s: JsonSchema, required: boolean): FieldControl {
  const initial = typeof s.default === "boolean" ? String(s.default) : required ? "false" : "";
  const node = selectInput(
    [
      ...(required ? [] : [{ value: "", label: "(not set)" }]),
      { value: "true", label: "true" },
      { value: "false", label: "false" },
    ],
    initial,
  );
  return {
    node,
    degraded: false,
    read() {
      if (node.value === "") {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and nothing is selected.` }
          : { ok: true, value: undefined };
      }
      return { ok: true, value: node.value === "true" };
    },
    write(value) {
      node.value = typeof value === "boolean" ? String(value) : "";
    },
  };
}

/** An array of strings, one per line — the same rule the arguments editor
 *  uses: a line is an element, never a split on whitespace. */
function stringArrayControl(name: string, required: boolean): FieldControl {
  const editor = linesEditor([], "one element per line");
  return {
    node: editor.node,
    degraded: false,
    read() {
      const items = editor.value();
      if (items.length === 0) {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and empty.` }
          : { ok: true, value: undefined };
      }
      return { ok: true, value: items };
    },
    write(value) {
      const area = editor.node as HTMLTextAreaElement;
      area.value = Array.isArray(value) ? value.map((v) => jsonScalarText(v)).join("\n") : "";
    },
  };
}

/** The escape hatch for a parameter this generator does not render: a nested
 *  object, an array of objects, a oneOf. The field is marked so the form as a
 *  whole can admit it is not the full picture. */
function rawFieldControl(name: string, required: boolean): FieldControl {
  const area = el("textarea", { class: "input textarea" }) as HTMLTextAreaElement;
  area.rows = 3;
  area.placeholder = "JSON value";
  return {
    node: area,
    degraded: true,
    read() {
      const text = area.value.trim();
      if (text === "") {
        return required
          ? { ok: false, fault: "missing", message: `“${name}” is required and empty.` }
          : { ok: true, value: undefined };
      }
      try {
        return { ok: true, value: JSON.parse(text) as unknown };
      } catch (err) {
        return {
          ok: false,
          fault: "invalid",
          message: `“${name}” is not valid JSON: ${err instanceof Error ? err.message : String(err)}`,
        };
      }
    },
    write(value) {
      area.value = value === undefined ? "" : JSON.stringify(value, null, 2);
    },
  };
}

function controlFor(name: string, s: JsonSchema, required: boolean): FieldControl {
  if (Array.isArray(s.enum) && s.enum.length > 0 && s.enum.every((v) => typeof v !== "object")) {
    return enumControl(name, s, required);
  }
  switch (schemaType(s)) {
    case "string":
      return stringControl(name, s, required);
    case "number":
      return numberControl(name, s, required, false);
    case "integer":
      return numberControl(name, s, required, true);
    case "boolean":
      return booleanControl(name, s, required);
    case "array": {
      const items = isRecord(s.items) ? (s.items as JsonSchema) : null;
      if (items && schemaType(items) === "string" && !Array.isArray(items.enum)) {
        return stringArrayControl(name, required);
      }
      return rawFieldControl(name, required);
    }
    default:
      return rawFieldControl(name, required);
  }
}

type ArgsResult = { ok: true; args: Record<string, unknown> } | { ok: false; message: string };

interface ArgsEditor {
  node: HTMLElement;
  /** The arguments to send, or why they cannot be built. */
  value(): ArgsResult;
  /** Everything filled in so far, without enforcing required-ness. Used when
   *  switching to raw mode: the user is leaving the form BECAUSE it cannot
   *  express what they want, so refusing to hand over a partial object would
   *  make the escape hatch unreachable exactly when it is needed. */
  partial(): ArgsResult;
  /** Refills the form from a raw object; reports keys the schema does not
   *  declare rather than dropping them. */
  fill(value: Record<string, unknown>): { unknown: string[] };
  /** True when at least one field fell back to raw JSON. */
  degraded: boolean;
  /** True when this tool declares no parameters at all. */
  empty: boolean;
}

function buildForm(check: Extract<SchemaCheck, { ok: true }>): ArgsEditor {
  const rows: { name: string; control: FieldControl; required: boolean }[] = [];
  const node = el("div", { class: "form" });
  for (const [name, sub] of check.properties) {
    const required = check.required.has(name);
    const control = controlFor(name, sub, required);
    rows.push({ name, control, required });
    const label = required ? `${name} *` : name;
    const hintParts = [
      sub.description ?? "",
      control.degraded ? "not renderable as a field — enter this one as JSON" : "",
    ].filter(Boolean);
    node.append(field(label, control.node, hintParts.join(" · ") || undefined));
  }
  const collect = (enforceRequired: boolean): ArgsResult => {
    const args: Record<string, unknown> = {};
    for (const r of rows) {
      const got = r.control.read();
      if (!got.ok) {
        if (!enforceRequired && got.fault === "missing") continue;
        return { ok: false, message: got.message };
      }
      if (got.value !== undefined) args[r.name] = got.value;
    }
    return { ok: true, args };
  };
  return {
    node,
    degraded: rows.some((r) => r.control.degraded),
    empty: rows.length === 0,
    value: () => collect(true),
    partial: () => collect(false),
    fill(value) {
      const known = new Set(rows.map((r) => r.name));
      for (const r of rows) r.control.write(value[r.name]);
      return { unknown: Object.keys(value).filter((k) => !known.has(k)) };
    },
  };
}

// ---------------------------------------------------------------------------
// Failure classification
// ---------------------------------------------------------------------------

/**
 * Does this failure look like the SERVER refusing the daemon's credentials?
 *
 * Fail direction: FAIL-OPEN toward saying nothing. An unrecognised failure
 * gets no authentication pointer, because sending someone to the Auth page
 * for a spawn failure costs them a detour and teaches them to ignore the
 * hint; missing a real 401 costs one extra hop they were going to take
 * anyway.
 *
 * Only the message and the daemon's own hint are matched — never the HTTP
 * status, which belongs to the CONTROL PLANE call. "The daemon refused the
 * GUI" and "the downstream server refused the daemon" are different problems
 * with different fixes, and conflating them would send the operator to log in
 * to the wrong thing.
 */
function looksLikeAuthFailure(e: CallError): boolean {
  const text = `${e.message ?? ""} ${e.hint ?? ""}`;
  // A bare "token" is deliberately NOT a trigger: "unexpected token" is one of
  // the most common substrings in any JSON or parser error, and matching it
  // would point a syntax failure at the login page.
  return /\b401\b|\b403\b|unauthori[sz]|forbidden|authenticat|credential|oauth|invalid[_ -]?token|access[_ -]token/i.test(
    text,
  );
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function playgroundPage(): Page {
  let root: HTMLElement | null = null;
  const slot = noticeSlot();

  let servers: Server[] | null = null;
  let listError: unknown = null;
  let serverId = "";

  let handshake: ServerTestResult | null = null;
  let defs: ServerTestTool[] = [];
  let toolName = "";
  let editor: ArgsEditor | null = null;
  let rawMode = false;
  const rawArea = el("textarea", { class: "input textarea" }) as HTMLTextAreaElement;
  rawArea.rows = 8;
  rawArea.placeholder = "{}";

  let inFlight: Cancellable<ServerTestResult> | null = null;
  let ticker: number | undefined;

  const connBox = el("div", { class: "playground-connection" });
  const toolBox = el("div", { class: "playground-tool" });
  const argsBox = el("div", { class: "playground-arguments" });
  const runBox = el("div", { class: "playground-run" });
  const resultBox = el("div", { class: "playground-result" });

  // -- arguments -------------------------------------------------------------

  /** The arguments as they stand, from whichever mode is showing. */
  function currentArgs(): ArgsResult {
    if (rawMode) {
      const text = rawArea.value.trim();
      if (text === "") return { ok: true, args: {} };
      let parsed: unknown;
      try {
        parsed = JSON.parse(text) as unknown;
      } catch (err) {
        return { ok: false, message: `The arguments are not valid JSON: ${err instanceof Error ? err.message : String(err)}` };
      }
      if (!isRecord(parsed)) {
        return { ok: false, message: "The arguments must be a JSON object (MCP passes tool arguments as an object)." };
      }
      return { ok: true, args: parsed };
    }
    if (!editor) return { ok: true, args: {} };
    return editor.value();
  }

  function argsText(): string {
    const got = currentArgs();
    return got.ok ? JSON.stringify(got.args) : rawArea.value.trim() || "{}";
  }

  function toForm(): void {
    if (!editor) return;
    const text = rawArea.value.trim();
    let parsed: Record<string, unknown> = {};
    if (text !== "") {
      try {
        const v = JSON.parse(text) as unknown;
        if (!isRecord(v)) {
          slot.say("The raw arguments are not a JSON object, so the form cannot be filled from them.", "warn");
          return;
        }
        parsed = v;
      } catch (err) {
        slot.say(
          `The raw arguments are not valid JSON, so the form cannot be filled from them: ${
            err instanceof Error ? err.message : String(err)
          }`,
          "warn",
        );
        return;
      }
    }
    const { unknown } = editor.fill(parsed);
    if (unknown.length > 0) {
      // Refusing the switch rather than silently discarding. The keys the
      // schema does not declare are precisely the ones the operator typed on
      // purpose — a tool whose real parameters are wider than its published
      // schema is a thing that happens, and losing them on a mode toggle
      // would look like the call simply not working.
      slot.say(
        `Staying in raw JSON: the form has no field for ${unknown.join(", ")}, and switching would drop ` +
          "them. Remove them first if you want the form.",
        "warn",
      );
      return;
    }
    slot.clear();
    rawMode = false;
    drawArgs();
  }

  function toRaw(): void {
    const got = editor ? editor.partial() : ({ ok: true, args: {} } as ArgsResult);
    if (!got.ok) {
      slot.say(`The form cannot be converted yet: ${got.message}`, "warn");
      return;
    }
    rawArea.value = JSON.stringify(got.args, null, 2);
    slot.clear();
    rawMode = true;
    drawArgs();
  }

  function modeSwitch(): HTMLElement {
    const group = el("div", { class: "segmented", role: "group", "aria-label": "Argument entry mode" });
    const mk = (label: string, active: boolean, go: () => void): HTMLButtonElement => {
      const b = el("button", { type: "button", text: label }) as HTMLButtonElement;
      b.setAttribute("aria-pressed", String(active));
      b.addEventListener("click", go);
      return b;
    };
    group.append(
      mk("Form", !rawMode, () => {
        if (rawMode) toForm();
      }),
      mk("Raw JSON", rawMode, () => {
        if (!rawMode) toRaw();
      }),
    );
    return group;
  }

  function drawArgs(): void {
    clear(argsBox);
    if (!toolName) return;
    const def = defs.find((d) => d.name === toolName);
    // "The daemon sent no definitions at all" and "this server declares no
    // schema for this tool" look identical from here and are not the same
    // fact. Only the second is a statement about the server.
    const check: SchemaCheck =
      defs.length === 0
        ? {
            ok: false,
            reason:
              "This daemon returned tool names without their definitions, so the GUI has no schema to " +
              "build a form from. Arguments go in as raw JSON.",
          }
        : readObjectSchema(def?.input_schema);
    if (check.ok && !editor) editor = buildForm(check);

    const body: (Node | null)[] = [];
    if (!check.ok) {
      // No usable schema: raw is the only mode, and the reason is stated
      // rather than left as an unexplained missing form.
      rawMode = true;
      body.push(el("p", { class: "hint", text: check.reason }));
      body.push(rawArea);
    } else if (rawMode) {
      body.push(modeSwitch(), rawArea);
    } else if (editor?.empty) {
      body.push(
        modeSwitch(),
        el("p", { class: "hint", text: "This tool declares no parameters — call it with no arguments." }),
      );
    } else {
      body.push(modeSwitch(), editor?.node ?? null);
      if (editor?.degraded) {
        body.push(
          el("p", {
            class: "hint",
            text:
              "One or more parameters are not representable as a plain field and are entered as JSON. " +
              "Raw JSON mode covers the whole object if that is easier.",
          }),
        );
      }
      body.push(
        el("p", {
          class: "hint",
          text: "* is required. An optional field left blank is left OUT of the call — it is not sent as an empty value.",
        }),
      );
    }
    argsBox.append(el("div", { class: "panel panel-inset" }, [el("h3", { text: "Arguments" }), ...body]));
  }

  // -- calling ---------------------------------------------------------------

  function stopTicker(): void {
    if (ticker !== undefined) window.clearInterval(ticker);
    ticker = undefined;
  }

  async function callTool(): Promise<void> {
    if (!serverId || !toolName || inFlight) return;
    const got = currentArgs();
    if (!got.ok) {
      slot.say(got.message, "warn");
      return;
    }
    slot.clear();
    clear(resultBox);

    const started = Date.now();
    const run = button("Calling… 0s", "btn btn-primary", () => undefined);
    run.disabled = true;
    const cancel = button("Cancel", "btn btn-secondary", () => {
      inFlight?.cancel();
    });
    clear(runBox);
    runBox.append(controls(run, cancel));
    ticker = window.setInterval(() => {
      run.textContent = `Calling… ${Math.round((Date.now() - started) / 1000)}s`;
    }, 1000);

    const asked = { server: serverId, tool: toolName };
    const promise = hub.testServer(asked.server, {
      tool: asked.tool,
      args: got.args,
      timeout_ms: CALL_TIMEOUT_MS,
    });
    inFlight = promise;
    // A result that arrives after the user moved to another server or tool is
    // dropped rather than shown under the wrong heading.
    const stillCurrent = (): boolean =>
      root !== null && serverId === asked.server && toolName === asked.tool;
    try {
      const res = await promise;
      if (stillCurrent()) showResult(res);
    } catch (err) {
      if (!stillCurrent()) {
        // Nothing to report into: the page moved on.
      } else if (isCancelled(err)) {
        slot.say(`Cancelled after ${Math.round((Date.now() - started) / 1000)}s. ${CANCEL_CAVEAT}`, "warn");
      } else {
        showFailure(err);
      }
    } finally {
      stopTicker();
      inFlight = null;
      drawRun();
    }
  }

  function drawRun(): void {
    clear(runBox);
    if (!toolName) return;
    runBox.append(
      controls(
        button(`Call ${toolName}`, "btn btn-primary", () => void callTool()),
        copyButton(() => cliCall(serverId, toolName, argsText()), "Copy CLI command", "btn btn-secondary"),
      ),
      el("p", {
        class: "hint",
        text:
          "This runs the tool for real on the downstream server. It is the daemon's self-test, not the " +
          "gateway: scope and the per-tool allow lists are NOT applied here, so a call " +
          "that works on this page is not proof that a client would be allowed to make it.",
      }),
    );
  }

  /** The result area. Three outcomes, three shapes: a tool that answered, a
   *  tool that answered with an error (a valid answer — the call worked), and
   *  a call that did not complete at all. */
  function showResult(res: ServerTestResult): void {
    clear(resultBox);
    const call = res.call;
    const raw = JSON.stringify(res, null, 2);
    if (!call) {
      resultBox.append(el("div", { class: "notice", text: "The server answered, but reported no call." }));
      return;
    }
    const header = call.is_error
      ? el("div", { class: "notice notice-warn" }, [
          el("div", {}, [
            el("span", { class: "badge badge-degraded", text: "tool error" }),
            el("span", { text: `  ${call.tool} ran and reported an error in ${call.millis} ms.` }),
          ]),
          el("div", {
            class: "warn-line",
            text: "The call itself succeeded — this is the tool's own answer, not a transport or credential failure.",
          }),
        ])
      : el("div", { class: "notice" }, [
          el("span", { class: "badge badge-healthy", text: "ok" }),
          el("span", { text: `  ${call.tool} returned in ${call.millis} ms.` }),
        ]);
    const text = call.text ?? "";
    resultBox.append(
      el("div", { class: "panel panel-inset" }, [
        el("h3", { text: "Result" }),
        header,
        text
          ? el("div", {}, [
              el("pre", { class: "raw-text", text }),
              controls(copyButton(() => text, "Copy output", "btn btn-secondary")),
            ])
          : el("p", { class: "hint", text: "The tool returned no text content." }),
        rawDetails(raw, "Show the raw result JSON"),
        el("p", {
          class: "hint",
          text: "Output is truncated by the daemon at 2 KiB — a tool that answers with a megabyte is not rendered whole.",
        }),
      ]),
    );
  }

  function showFailure(err: unknown): void {
    clear(resultBox);
    const e = asCallError(err);
    const node = el("div", { class: "panel panel-inset" }, [
      el("h3", { text: "The call did not complete" }),
      failureBox(err),
    ]);
    if (looksLikeAuthFailure(e)) node.append(authPointer());
    resultBox.append(node);
  }

  /** Where to go when the failure smells like credentials. */
  function authPointer(): HTMLElement {
    return el("div", { class: "notice notice-warn" }, [
      el("div", {
        text: `${serverId} looks like it is refusing the credentials rather than failing to start.`,
      }),
      controls(
        el("a", { class: "btn", href: "#/auth", text: "Go to Auth" }),
        el("a", { class: "btn btn-secondary", href: "#/secrets", text: "Go to Secrets" }),
      ),
      el("span", {
        class: "hint",
        text:
          "An OAuth server needs a login; an API-key server needs its secret set. agenthub can send a " +
          "credential and can prove it works by calling — it can never read one back to show you.",
      }),
    ]);
  }

  // -- tools -----------------------------------------------------------------

  /** Abandons a call the user has navigated away from.
   *
   *  Without this, moving to another tool mid-call leaves an invisible call
   *  in flight: its Cancel button has been redrawn away, and the guard at the
   *  top of callTool would then refuse the next click in silence. Cancelling
   *  is also the honest reading of the gesture — nobody switches tools while
   *  still waiting for the previous answer. */
  function abandonInFlight(): void {
    if (!inFlight) return;
    inFlight.cancel();
    inFlight = null;
    stopTicker();
  }

  function selectTool(name: string): void {
    abandonInFlight();
    toolName = name;
    editor = null;
    rawMode = false;
    rawArea.value = "";
    slot.clear();
    clear(resultBox);
    drawToolDetail();
    drawArgs();
    drawRun();
  }

  const toolDetail = el("div", {});

  function drawToolDetail(): void {
    clear(toolDetail);
    const def = defs.find((d) => d.name === toolName);
    if (!def) return;
    toolDetail.append(
      el("div", {}, [
        el("div", { class: "mono", text: def.signature || def.name }),
        def.description ? el("p", { class: "hint", text: def.description }) : null,
        def.lossy
          ? el("p", {
              class: "hint",
              text:
                "The signature above is abbreviated — it dropped part of the schema. The form below is " +
                "generated from the full schema, so trust the form over the signature.",
            })
          : null,
      ]),
    );
  }

  function drawTools(): void {
    clear(toolBox);
    if (!handshake) return;
    if (defs.length === 0 && handshake.tools.length === 0) {
      toolBox.append(
        emptyState({
          kind: "empty",
          title: `${handshake.server} connected but offers no tools.`,
          body: "The handshake succeeded and the tool list came back empty. That is the server's answer, not a failure to ask.",
        }),
      );
      return;
    }
    // A daemon that predates the definitions field still lists names; the
    // page degrades to raw arguments rather than to nothing.
    const names = defs.length > 0 ? defs.map((d) => d.name) : handshake.tools;
    const picker = selectInput(
      [
        { value: "", label: `Pick one of ${names.length} tools…` },
        ...names.map((n) => {
          const def = defs.find((d) => d.name === n);
          return { value: n, label: def?.signature ? def.signature : n };
        }),
      ],
      toolName,
    );
    picker.addEventListener("change", () => selectTool(picker.value));
    toolBox.append(
      el("div", { class: "panel panel-inset" }, [
        el("h3", { text: "Tool" }),
        field("Tool", picker, "Named with the ORIGINAL downstream name, not the name a client sees after overrides."),
        toolDetail,
        defs.length === 0
          ? el("p", {
              class: "hint",
              text:
                "This daemon returned names without definitions, so there are no schemas to build a form " +
                "from — arguments are entered as raw JSON below.",
            })
          : null,
      ]),
    );
    drawToolDetail();
  }

  function connectionView(res: ServerTestResult): HTMLElement {
    const kv = (k: string, v: string): HTMLElement =>
      el("div", { class: "kv" }, [
        el("span", { class: "k", text: k }),
        el("span", { class: "v", text: v }),
      ]);
    return el("div", { class: "kvs" }, [
      kv("Transport", res.transport),
      kv("Server", res.server_info || "—"),
      kv("Protocol", res.protocol_version || "—"),
      kv("Connected in", `${res.connect_ms} ms`),
      kv("Tools", String(res.tool_count)),
    ]);
  }

  async function loadTools(): Promise<void> {
    if (!serverId) return;
    abandonInFlight();
    handshake = null;
    defs = [];
    toolName = "";
    editor = null;
    rawMode = false;
    clear(toolBox);
    clear(argsBox);
    clear(runBox);
    clear(resultBox);
    slot.clear();
    clear(connBox);
    connBox.append(loadingState(`Connecting to ${serverId}…`, 2));

    const asked = serverId;
    try {
      const res = await hub.testServer(asked, { defs: true, timeout_ms: CALL_TIMEOUT_MS });
      // The page may have been disposed, or the user may have moved on to
      // another server, while this handshake was in flight. Rendering it
      // anyway would label one server's tools with another server's name.
      if (!root || serverId !== asked) return;
      handshake = res;
      defs = res.tool_defs ?? [];
      clear(connBox);
      connBox.append(
        el("div", { class: "panel panel-inset" }, [
          el("h3", { text: `${res.server} answered` }),
          connectionView(res),
          controls(copyButton(() => cliList(serverId), "Copy CLI command", "btn btn-secondary")),
        ]),
      );
      drawTools();
    } catch (err) {
      if (!root || serverId !== asked) return;
      clear(connBox);
      const e = asCallError(err);
      const box = el("div", { class: "panel panel-inset" }, [
        el("h3", { text: `${asked} did not answer` }),
        failureBox(err),
      ]);
      if (looksLikeAuthFailure(e)) box.append(authPointer());
      connBox.append(box);
    }
  }

  // -- server picker ---------------------------------------------------------

  function picker(): HTMLElement {
    const list = servers ?? [];
    const options = [
      { value: "", label: "Pick a server…" },
      ...list.map((s) => ({
        value: s.id,
        label: s.enabled ? s.id : `${s.id} (disabled)`,
      })),
    ];
    const select = selectInput(options, serverId);
    select.addEventListener("change", () => {
      abandonInFlight();
      serverId = select.value;
      clear(connBox);
      clear(toolBox);
      clear(argsBox);
      clear(runBox);
      clear(resultBox);
      if (serverId) void loadTools();
    });
    return el("div", { class: "form-inline" }, [
      field("Server", select, "A disabled server can still be probed here — the probe does not go through the gateway."),
      button("Reconnect", "btn", () => void loadTools()),
    ]);
  }

  async function draw(): Promise<void> {
    if (!root) return;
    clear(root);
    root.append(
      pageHeader(
        "Playground",
        "Connect to one downstream server, inspect its real schema, and run a tool without wiring a client first.",
      ),
      loadingState("Reading the server list…", 3),
    );
    try {
      servers = await hub.listServers();
      listError = null;
    } catch (err) {
      servers = null;
      listError = err;
    }
    if (!root) return;
    clear(root);

    if (listError) {
      root.append(
        pageHeader(
          "Playground",
          "Connect to one downstream server, inspect its real schema, and run a tool without wiring a client first.",
        ),
        failureState(listError, "the server list", () => void draw()),
      );
      return;
    }
    if ((servers ?? []).length === 0) {
      root.append(
        pageHeader(
          "Playground",
          "Connect to one downstream server, inspect its real schema, and run a tool without wiring a client first.",
        ),
        emptyState({
          kind: "empty",
          title: "No servers to test",
          body:
            "The playground calls a tool on a server this machine already knows about, and there are " +
            "none registered yet.",
          actions: [el("a", { class: "btn btn-primary", href: "#/servers", text: "Add a server" })],
        }),
      );
      return;
    }

    root.append(
      pageHeader(
        "Playground",
        "Connect to one downstream server, inspect its real schema, and run a tool without wiring a client first.",
      ),
      el("div", { class: "direct-test-banner" }, [
        el("span", { class: "direct-test-label", text: "DIRECT DOWNSTREAM TEST" }),
        el("span", {
          text:
            "This bypasses Client/Profile scope and global tool allow-lists. Success proves the server works; it does not prove a client is allowed to call it.",
        }),
      ]),
      slot.node,
      el("div", { class: "playground-picker" }, [picker()]),
      el("div", { class: "playground-grid" }, [
        el("section", { class: "playground-setup" }, [
          el("div", { class: "workspace-label", text: "Setup and arguments" }),
          connBox,
          toolBox,
          argsBox,
          runBox,
        ]),
        el("section", { class: "playground-output" }, [
          el("div", { class: "workspace-label", text: "Result" }),
          resultBox,
          el("div", { class: "result-placeholder" }, [
            el("span", { class: "result-placeholder-mark", "aria-hidden": "true" }, [icon("play")]),
            el("strong", { text: "Run a tool to inspect its response" }),
            el("span", { text: "Tool output, timing and raw JSON will appear here." }),
          ]),
        ]),
      ]),
    );
    if (serverId) void loadTools();
  }

  return {
    render(node) {
      root = node;
      return draw();
    },
    dispose() {
      // A call left in flight is abandoned rather than left to resolve into
      // a page that is no longer on screen.
      abandonInFlight();
      stopTicker();
      root = null;
      servers = null;
      handshake = null;
      defs = [];
      toolName = "";
      editor = null;
    },
  };
}
