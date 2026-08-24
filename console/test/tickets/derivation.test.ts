// test/tickets/derivation.test.ts — pure-function units for the Tickets views
// (05-testing §3.2(a)/(c)/(d)/(e)/(f)): §13 board-derivation, §8.6
// blocked-is-a-condition, RBAC drag gate, per-column sort incl. Updated,
// shared filter narrowing, and view-toggle persistence.

import { describe, it, expect } from "vitest";
import {
  applyFilters,
  canDrag,
  deriveColumns,
  distinctLabels,
  distinctValues,
  filtersToParams,
  isBlocked,
  isRoot,
  nextSortDir,
  sortWorkItems,
} from "@/lib/tickets/derivation";
import { resolveInitialView, persistView } from "@/lib/tickets/viewState";
import type { WorkItem } from "@/lib/tickets/types";

function item(partial: Partial<WorkItem> & Pick<WorkItem, "id">): WorkItem {
  return {
    projectId: "p1",
    parentId: null,
    title: partial.title ?? `item ${partial.id}`,
    state: "backlog",
    blockedReason: null,
    updatedAt: "2026-08-01T00:00:00Z",
    ...partial,
  };
}

describe("deriveColumns — §13 board-derivation is a PURE projection of state", () => {
  it("always returns exactly the five canonical columns in order, even when empty", () => {
    const cols = deriveColumns([]);
    expect(cols.map((c) => c.state)).toEqual([
      "backlog",
      "todo",
      "in_progress",
      "in_review",
      "done",
    ]);
  });

  it("places each item by state ONLY — a state-only fixture change moves the card", () => {
    const before = deriveColumns([item({ id: "a", state: "todo" })]);
    expect(before.find((c) => c.state === "todo")!.items.map((i) => i.id)).toEqual(["a"]);
    // The ONLY change is `state` — no `column` field exists to consult.
    const after = deriveColumns([item({ id: "a", state: "in_review" })]);
    expect(after.find((c) => c.state === "todo")!.items).toHaveLength(0);
    expect(after.find((c) => c.state === "in_review")!.items.map((i) => i.id)).toEqual(["a"]);
  });

  it("a blocked item stays in its workflow lane — never a 6th column (§8.6)", () => {
    const cols = deriveColumns([
      item({ id: "b", state: "in_progress", blockedReason: "needs_approval" }),
    ]);
    expect(cols).toHaveLength(5);
    expect(cols.find((c) => c.state === "in_progress")!.items).toHaveLength(1);
    expect(cols.some((c) => (c.state as string) === "blocked")).toBe(false);
    expect(isBlocked(cols[2].items[0])).toBe(true);
  });
});

describe("canDrag — UI RBAC gate mirrors the §6.7.2 server wall", () => {
  it.each(["viewer", undefined, null, "unknown", ""])("role %p ⇒ NO drag", (role) => {
    expect(canDrag(role as string)).toBe(false);
  });
  it.each(["contributor", "maintainer"])("role %p ⇒ drag enabled", (role) => {
    expect(canDrag(role)).toBe(true);
  });
});

describe("sortWorkItems — per-column asc/desc incl. Updated (8.14c AC2)", () => {
  const rows = [
    item({ id: "c-3", title: "Alpha", updatedAt: "2026-08-03T00:00:00Z", priority: "high" }),
    item({ id: "a-1", title: "beta", updatedAt: "2026-08-01T00:00:00Z", priority: null }),
    item({ id: "b-2", title: "Cyan", updatedAt: "2026-08-02T00:00:00Z", priority: "low" }),
  ];

  it("sorts by Updated asc and desc (recency via updated_at)", () => {
    const asc = sortWorkItems(rows, { key: "updated", dir: "asc" });
    expect(asc.map((r) => r.id)).toEqual(["a-1", "b-2", "c-3"]);
    const desc = sortWorkItems(rows, { key: "updated", dir: "desc" });
    expect(desc.map((r) => r.id)).toEqual(["c-3", "b-2", "a-1"]);
  });

  it("sorts by title case-insensitively", () => {
    const asc = sortWorkItems(rows, { key: "title", dir: "asc" });
    expect(asc.map((r) => r.title)).toEqual(["Alpha", "beta", "Cyan"]);
  });

  it("missing values sort LAST in both directions — deterministic, never interleaved", () => {
    const asc = sortWorkItems(rows, { key: "priority", dir: "asc" });
    expect(asc.map((r) => r.priority)).toEqual(["high", "low", null]);
    const desc = sortWorkItems(rows, { key: "priority", dir: "desc" });
    expect(desc.map((r) => r.priority)).toEqual(["low", "high", null]);
  });

  it("nextSortDir toggles asc ⇄ desc on the active column and resets on a new column", () => {
    const first = nextSortDir({ key: "id", dir: "asc" }, "title");
    expect(first).toEqual({ key: "title", dir: "asc" });
    const toggled = nextSortDir(first, "title");
    expect(toggled).toEqual({ key: "title", dir: "desc" });
  });
});

describe("applyFilters — the SAME narrowing for both views (8.14d AC1)", () => {
  const rows = [
    item({ id: "11111111-1111-1111-1111-111111111111", title: "Fix login", priority: "high", assignee: "ada", labels: ["bug"] }),
    item({ id: "22222222-2222-2222-2222-222222222222", title: "Add docs", priority: "low", assignee: "bob", labels: ["docs"] }),
  ];

  it("empty filters are a no-op passthrough", () => {
    expect(applyFilters(rows, { query: "", priority: "", assignee: "", label: "" })).toHaveLength(2);
  });

  it("query matches title OR id, case-insensitively", () => {
    expect(applyFilters(rows, { query: "login", priority: "", assignee: "", label: "" })[0].title).toBe("Fix login");
    expect(applyFilters(rows, { query: "11111111", priority: "", assignee: "", label: "" })[0].title).toBe("Fix login");
    expect(applyFilters(rows, { query: "nope", priority: "", assignee: "", label: "" })).toHaveLength(0);
  });

  it("contextual filters compose (priority + assignee + label)", () => {
    expect(
      applyFilters(rows, { query: "", priority: "high", assignee: "ada", label: "bug" }),
    ).toHaveLength(1);
    expect(
      applyFilters(rows, { query: "", priority: "high", assignee: "bob", label: "" }),
    ).toHaveLength(0);
  });

  it("distinctValues/distinctLabels feed the filter selects from loaded data", () => {
    expect(distinctValues(rows, "priority")).toEqual(["high", "low"]);
    expect(distinctValues(rows, "assignee")).toEqual(["ada", "bob"]);
    expect(distinctLabels(rows)).toEqual(["bug", "docs"]);
  });

  it("filtersToParams carries the server-side, tenancy-scoped predicates (§12.1)", () => {
    const qs = filtersToParams({ query: "login", priority: "high", assignee: "", label: "bug" });
    expect(new URLSearchParams(qs).get("q")).toBe("login");
    expect(new URLSearchParams(qs).get("priority")).toBe("high");
    expect(new URLSearchParams(qs).get("label")).toBe("bug");
    expect(new URLSearchParams(qs).get("assignee")).toBeNull();
  });
});

describe("isRoot — orphans are roots by construction (§6.1, 8.17 AC4)", () => {
  it("parent_id NULL ⇒ root (an orphan's parent was SET NULL on delete)", () => {
    expect(isRoot(item({ id: "r" }))).toBe(true);
    expect(isRoot(item({ id: "c", parentId: "r" }))).toBe(false);
    // An orphan whose parent row vanished reads parent_id = NULL from the read
    // model — it renders as a root, never hidden behind a missing parent.
    expect(isRoot(item({ id: "orphan", parentId: null }))).toBe(true);
  });
});

describe("view persistence (8.14d AC3)", () => {
  const memory = () => {
    let store: Record<string, string> = {};
    return {
      getItem: (k: string) => store[k] ?? null,
      setItem: (k: string, v: string) => void (store[k] = v),
      removeItem: (k: string) => delete store[k],
    } as unknown as Storage;
  };

  it("?view= param WINS over localStorage (explicit deep-link intent)", () => {
    const s = memory();
    persistView("kanban", s);
    expect(resolveInitialView("?view=list", s)).toBe("list");
  });

  it("falls back to localStorage, then to Kanban default", () => {
    const s = memory();
    persistView("list", s);
    expect(resolveInitialView("", s)).toBe("list");
    expect(resolveInitialView("", memory())).toBe("kanban");
  });

  it("garbage values never win — always a valid view", () => {
    const s = memory();
    s.setItem("ksq.tickets.view", "board");
    expect(resolveInitialView("?view=grid", s)).toBe("kanban");
  });
});
