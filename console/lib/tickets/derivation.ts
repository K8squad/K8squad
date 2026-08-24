// lib/tickets/derivation.ts — PURE functions for the Tickets views (8.14b–d/8.17).
//
// Everything here is side-effect free so the §13 board-derivation, the §8.6
// blocked-is-a-condition rule, sorting, filtering and view persistence are all
// unit-testable without a DOM or network (05-testing §3.2(a)–(f)). The Kanban
// columns are DERIVED from `work_item.state` only — there is no stored `column`
// field to consult anywhere in this module.

import {
  WORK_ITEM_STATES,
  type SortDir,
  type SortKey,
  type SortSpec,
  type TicketFilters,
  type WorkItem,
  type WorkItemState,
} from "./types";

/** A board column — a pure projection of state (§13), always all five, even empty. */
export interface BoardColumn {
  state: WorkItemState;
  items: WorkItem[];
}

/**
 * Derive the five board columns from `state` ONLY. A fixture change to `state`
 * moves the card (8.14b AC1); a `blockedReason` never changes the lane (§8.6).
 */
export function deriveColumns(items: readonly WorkItem[]): BoardColumn[] {
  const byState = new Map<WorkItemState, WorkItem[]>(
    WORK_ITEM_STATES.map((s) => [s, []]),
  );
  for (const item of items) {
    const lane = byState.get(item.state);
    if (lane) lane.push(item); // unknown states are dropped, not guessed into a lane
  }
  return WORK_ITEM_STATES.map((state) => ({
    state,
    items: byState.get(state)!,
  }));
}

/** A blocked item renders in its workflow lane with a badge overlay (§8.6). */
export function isBlocked(item: WorkItem): boolean {
  return item.blockedReason != null;
}

/** Only the human status-transition is mutable on this screen (R6 scope guard). */
export function canDrag(role: string | undefined | null): boolean {
  return role === "contributor" || role === "maintainer";
}

/** Roots of the tree: `parent_id IS NULL` — an orphan (ON DELETE SET NULL) IS a root (§6.1). */
export function isRoot(item: WorkItem): boolean {
  return item.parentId == null;
}

const COLLATOR = new Intl.Collator("en", { sensitivity: "base", numeric: true });

function compareValues(
  a: string | undefined,
  b: string | undefined,
  dir: SortDir,
): number {
  // Deterministic ordering: missing values ALWAYS sort last, in BOTH directions.
  // The direction flips only the ordering of PRESENT values — negating the whole
  // comparison would also flip the missing-last sentinel and float nulls to the top.
  const aMissing = a == null || a === "";
  const bMissing = b == null || b === "";
  if (aMissing && bMissing) return 0;
  if (aMissing) return 1;
  if (bMissing) return -1;
  const cmp = COLLATOR.compare(a, b);
  return dir === "asc" ? cmp : -cmp;
}

function sortValue(item: WorkItem, key: SortKey): string | undefined {
  switch (key) {
    case "id":
      return item.id;
    case "title":
      return item.title;
    case "status":
      return String(WORK_ITEM_STATES.indexOf(item.state)).padStart(2, "0");
    case "priority":
      return item.priority ?? undefined;
    case "assignee":
      return item.assignee ?? undefined;
    case "labels":
      return (item.labels ?? []).join(" ");
    case "updated":
      return item.updatedAt; // ISO-8601 sorts lexicographically
  }
}

/** Sort by any column asc/desc, incl. Updated recency (8.14c AC2). Stable + deterministic. */
export function sortWorkItems(
  items: readonly WorkItem[],
  { key, dir }: SortSpec,
): WorkItem[] {
  const out = [...items];
  out.sort((a, b) => compareValues(sortValue(a, key), sortValue(b, key), dir));
  return out;
}

/** Toggle helper for the column-header buttons (aria-sort asc ⇄ desc). */
export function nextSortDir(
  current: SortSpec,
  key: SortKey,
): SortSpec {
  if (current.key !== key) return { key, dir: "asc" };
  return { key, dir: current.dir === "asc" ? "desc" : "asc" };
}

function matchesFilter(item: WorkItem, filters: TicketFilters): boolean {
  const q = filters.query.trim().toLowerCase();
  if (q !== "") {
    const inTitle = item.title.toLowerCase().includes(q);
    const inId = item.id.toLowerCase().includes(q);
    if (!inTitle && !inId) return false;
  }
  if (filters.priority !== "" && (item.priority ?? "") !== filters.priority) {
    return false;
  }
  if (filters.assignee !== "" && (item.assignee ?? "") !== filters.assignee) {
    return false;
  }
  if (filters.label !== "" && !(item.labels ?? []).includes(filters.label)) {
    return false;
  }
  return true;
}

/**
 * Apply the shared search/filters to a set of items — the SAME narrowing for both
 * views (8.14d AC1). Applied client-side over the server-returned (already
 * tenancy-scoped, server-side-filtered) set so Kanban and List can never disagree.
 * A parent that matches keeps its children visible only if they match too —
 * filtering is per-item, not per-subtree (scan/sort semantics, not collapse).
 */
export function applyFilters(
  items: readonly WorkItem[],
  filters: TicketFilters,
): WorkItem[] {
  if (
    filters.query.trim() === "" &&
    filters.priority === "" &&
    filters.assignee === "" &&
    filters.label === ""
  ) {
    return [...items];
  }
  return items.filter((item) => matchesFilter(item, filters));
}

/** Server-side query params for the BFF read (§12.1 indexed predicates, 8.14d). */
export function filtersToParams(
  filters: TicketFilters,
  extra?: Record<string, string>,
): string {
  const params = new URLSearchParams();
  if (filters.query.trim() !== "") params.set("q", filters.query.trim());
  if (filters.priority !== "") params.set("priority", filters.priority);
  if (filters.assignee !== "") params.set("assignee", filters.assignee);
  if (filters.label !== "") params.set("label", filters.label);
  if (extra) {
    for (const [k, v] of Object.entries(extra)) params.set(k, v);
  }
  return params.toString();
}

/** Distinct filter option values present in the loaded set (for the selects). */
export function distinctValues(
  items: readonly WorkItem[],
  field: "priority" | "assignee",
): string[] {
  const seen = new Set<string>();
  for (const item of items) {
    const v = item[field];
    if (v != null && v !== "") seen.add(v);
  }
  return [...seen].sort(COLLATOR.compare);
}

export function distinctLabels(items: readonly WorkItem[]): string[] {
  const seen = new Set<string>();
  for (const item of items) for (const l of item.labels ?? []) seen.add(l);
  return [...seen].sort(COLLATOR.compare);
}
