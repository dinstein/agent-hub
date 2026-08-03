// What the three observability pages — Calls, Events, Logs — share.
//
// They answer three questions about one installation, and somebody comparing
// them is switching between the three inside a minute: a time range that
// means the same thing, a filter bar in the same place, rows newest first,
// and one pager that behaves identically. Three implementations of that were
// three chances to be subtly different in the one place a reader would not
// think to check.
//
// The daemon holds up its half: every list is newest first and paged by a
// cursor naming the last row served (`internal/ctlapi/pagecursor.go`). An
// offset would repeat rows as new records arrive — page two after ten new
// records shows ten it already showed — which is exactly the failure a person
// reading a busy log would blame on the hub rather than on the pager.

import { el } from "../dom";
import { selectInput } from "../ui";

/** Ranges every observability page offers, in hours. 0 is "everything". */
export const RANGES: { label: string; hours: number }[] = [
  { label: "Last hour", hours: 1 },
  { label: "Last 24 hours", hours: 24 },
  { label: "Last 7 days", hours: 24 * 7 },
  { label: "Everything", hours: 0 },
];

/** rangeMillis turns a chosen range into the `since` the API wants. */
export function rangeMillis(hours: number): number {
  return hours > 0 ? Date.now() - hours * 3600_000 : 0;
}

/** One labelled control in a filter bar. */
export function filterField(label: string, control: HTMLElement): HTMLElement {
  return el("label", { class: "activity-filter-field" }, [el("span", { text: label }), control]);
}

/** The filter bar itself, so the three pages put their controls in the same
 *  place and at the same size. */
export function filterBar(...fields: (HTMLElement | null)[]): HTMLElement {
  return el("div", { class: "activity-toolbar" }, fields.filter(Boolean) as HTMLElement[]);
}

/** rangeField is the time control every page has, wired to a callback. */
export function rangeField(hours: number, onChange: (hours: number) => void): HTMLElement {
  const select = selectInput(
    RANGES.map((r) => ({ value: String(r.hours), label: r.label })),
    String(hours),
  );
  select.addEventListener("change", () => onChange(Number(select.value)));
  return filterField("Time range", select);
}

/** Pager is the cursor walk one page owns.
 *
 *  It remembers the cursor of every page VISITED rather than only the next
 *  one, which is what makes Previous work at all: a cursor names a position
 *  in one direction, so going back means re-reading from a cursor already
 *  held, not inverting one. */
export class Pager {
  index = 0;
  /** cursors[i] is the cursor that produced page i; page 0 has none. */
  private cursors: string[] = [""];
  /** next is the cursor for the page AFTER the current one, "" at the end. */
  next = "";

  /** current is the cursor to request for the page being shown. */
  current(): string {
    return this.cursors[this.index] ?? "";
  }

  /** reset returns to page one. Every filter change does this: a cursor
   *  taken under one filter names a row the next filter may not contain, and
   *  paging on regardless would silently skip records. */
  reset(): void {
    this.index = 0;
    this.cursors = [""];
    this.next = "";
  }

  /** accept records the page just loaded. */
  accept(nextCursor: string | undefined): void {
    this.next = nextCursor ?? "";
  }

  canPrev(): boolean {
    return this.index > 0;
  }

  canNext(): boolean {
    return this.next !== "";
  }

  prev(): void {
    if (this.index > 0) this.index--;
  }

  advance(): void {
    if (!this.next) return;
    this.index++;
    this.cursors[this.index] = this.next;
    // Anything beyond the new page is unreachable from here and would be a
    // stale position if the filters moved under us.
    this.cursors.length = this.index + 1;
  }
}

/** pagerFooter renders the shared "showing X–Y of N" line plus its buttons.
 *
 *  `total` is what the daemon counted from the cursor onward, so on page one
 *  it is the whole matching set. It is shown rather than inferred because
 *  "0 of 0" and "0 of 4213 with these filters" send a reader to different
 *  places. */
export function pagerFooter(o: {
  pager: Pager;
  pageSize: number;
  shown: number;
  total: number;
  noun: string;
  onChange: () => void;
}): HTMLElement {
  const previous = el("button", { class: "btn btn-sm", type: "button", text: "Previous" });
  previous.disabled = !o.pager.canPrev();
  previous.addEventListener("click", () => {
    if (!o.pager.canPrev()) return;
    o.pager.prev();
    o.onChange();
  });
  const next = el("button", { class: "btn btn-sm", type: "button", text: "Next" });
  next.disabled = !o.pager.canNext();
  next.addEventListener("click", () => {
    if (!o.pager.canNext()) return;
    o.pager.advance();
    o.onChange();
  });
  const first = o.shown === 0 ? 0 : o.pager.index * o.pageSize + 1;
  const last = o.shown === 0 ? 0 : first + o.shown - 1;
  const counted = o.pager.index === 0 ? o.total : o.total + o.pager.index * o.pageSize;
  return el("footer", { class: "activity-pagination" }, [
    el("span", {
      text: o.shown ? `${first}–${last} of ${counted} ${o.noun}` : `0 ${o.noun}`,
    }),
    el("div", { class: "activity-page-actions" }, [previous, next]),
  ]);
}
