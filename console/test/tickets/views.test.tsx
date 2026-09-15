// test/tickets/views.test.tsx — component-render units for the Tickets views
// (05-testing §3.2(b) blocked-badge, §3.2(d) List sort/tree, §3.2 8.17 tree).
//
// These cover what the pure-function units in derivation.test.ts cannot: that
// the List view (Paperclip row bands, ISI-4452 S2) renders the sub-ticket tree
// as sibling row bands, that carets/count-badges follow the 8.17 leaf-vs-parent
// rule, that expanding reveals child rows, that the phase-status chip + colour
// legend (S1/S5) render, and that the Kanban blocked overlay renders in the
// item's own lane (§8.6).

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, within } from "@testing-library/react";
import { ListView } from "@/components/tickets/ListView";
import { KanbanBoard } from "@/components/tickets/KanbanBoard";
import type { TreeController } from "@/components/tickets/SubTicketTree";
import type { SortSpec, WorkItem } from "@/lib/tickets/types";
import type { PhaseStatus } from "@/lib/tickets/statusColor";

afterEach(cleanup);

// The read-model type is still the legacy 5-value enum (ISI-4456 kept it narrow),
// but live data + the Kanban target the ISI-4455 ten-status enum — accept any
// state string here and assert onto WorkItem["state"] at the one build seam.
function item(
  partial: Partial<Omit<WorkItem, "state">> & Pick<WorkItem, "id"> & { state?: string },
): WorkItem {
  const { state, ...rest } = partial;
  return {
    projectId: "p1",
    parentId: null,
    title: partial.title ?? `item ${partial.id}`,
    state: (state ?? "backlog") as WorkItem["state"],
    blockedReason: null,
    updatedAt: "2026-08-01T00:00:00Z",
    ...rest,
  };
}

/** A controller with a fixed expanded set + preloaded children (no network). */
function controller(opts: {
  expanded?: Set<string>;
  children?: Record<string, WorkItem[]>;
  toggle?: (id: string) => void;
}): TreeController {
  const children = opts.children ?? {};
  const expanded = opts.expanded ?? new Set<string>();
  return {
    childrenOf: (pid) => children[pid],
    childCountOf: (it) => it.childCount ?? children[it.id]?.length ?? 0,
    isExpanded: (pid) => expanded.has(pid),
    toggle: opts.toggle ?? (() => {}),
  };
}

const SORT: SortSpec = { key: "updated", dir: "desc" };

describe("ListView — Paperclip row bands + sub-ticket tree (8.14c + 8.17)", () => {
  it("renders an expanded parent and its child as sibling row bands", () => {
    const parent = item({ id: "par", title: "Parent", childCount: 1 });
    const child = item({ id: "kid", title: "Child", parentId: "par" });
    render(
      <ListView
        items={[parent]}
        tree={controller({ expanded: new Set(["par"]), children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    // Bands are siblings under the same rows container (no table wrapper).
    const parentRow = screen.getByTestId("row-par");
    const childRow = screen.getByTestId("row-kid");
    expect(parentRow.parentElement).toBe(childRow.parentElement);
    expect(childRow).toBeInTheDocument();
    // The child band carries its depth for the indent/connector (8.17).
    expect(childRow.getAttribute("data-tree-depth")).toBe("1");
  });

  it("tints the status chip + spine and renders the phase label (S1)", () => {
    render(
      <ListView
        items={[item({ id: "d", state: "done" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("row-state-d")).toHaveTextContent("Done");
    // The row exposes the phase hue as a CSS custom property for spine/dot/chip.
    expect(screen.getByTestId("row-d").getAttribute("style")).toContain("--phase-hue");
  });

  it("shows a warn glyph on a blocked row and struck styling class on cancelled", () => {
    render(
      <ListView
        items={[
          item({ id: "b", state: "in_progress", blockedReason: "needs_approval" }),
          item({ id: "x", state: "cancelled" as unknown as WorkItem["state"] }),
        ]}
        tree={controller({})}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("blocked-glyph-b")).toBeInTheDocument();
    expect(screen.getByTestId("row-x").className).toContain("ksq-listrow--cancelled");
  });

  it("renders a 10-status colour legend in the footer (S5)", () => {
    render(
      <ListView
        items={[item({ id: "a" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("status-legend")).toBeInTheDocument();
    expect(screen.getByTestId("legend-code_review")).toHaveTextContent("Code Review");
    expect(screen.getByTestId("legend-blocked")).toBeInTheDocument();
  });

  it("a parent shows a caret + child-count badge; a leaf shows neither (8.17 AC1)", () => {
    render(
      <ListView
        items={[item({ id: "par", childCount: 3 }), item({ id: "leaf" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("tree-caret-par")).toBeInTheDocument();
    expect(screen.getByTestId("child-count-par")).toHaveTextContent("3");
    expect(screen.queryByTestId("tree-caret-leaf")).toBeNull();
  });

  it("collapsed parent hides its children until expanded", () => {
    const parent = item({ id: "par", childCount: 1 });
    const child = item({ id: "kid", parentId: "par" });
    const { rerender } = render(
      <ListView
        items={[parent]}
        tree={controller({ children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.queryByTestId("row-kid")).toBeNull();
    rerender(
      <ListView
        items={[parent]}
        tree={controller({ expanded: new Set(["par"]), children: { par: [child] } })}
        sort={SORT}
        onSortChange={vi.fn()}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("row-kid")).toBeInTheDocument();
  });

  it("clicking a column header requests the toggled sort spec (8.14c AC2)", () => {
    const onSortChange = vi.fn();
    render(
      <ListView
        items={[item({ id: "a" })]}
        tree={controller({})}
        sort={SORT}
        onSortChange={onSortChange}
        projectId="ns/demo"
      />,
    );
    fireEvent.click(screen.getByTestId("sort-title"));
    expect(onSortChange).toHaveBeenCalledWith({ key: "title", dir: "asc" });
  });
});

describe("KanbanBoard — phase columns, blocked overlay in-lane, RBAC drag gate (ISI-4457)", () => {
  const noop = vi.fn(async () => {});

  it("renders 10 phase columns grouped under the four super-headers", () => {
    render(
      <KanbanBoard
        items={[]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    for (const phase of [
      "backlog",
      "todo",
      "design",
      "planning",
      "implementation",
      "code_review",
      "testing",
      "documentation",
      "done",
      "cancelled",
    ]) {
      expect(screen.getByTestId(`column-${phase}`)).toBeInTheDocument();
    }
    for (const group of ["Intake", "Build", "Review", "Done"]) {
      expect(screen.getByTestId(`phase-group-${group}`)).toBeInTheDocument();
    }
  });

  it("folds a transitional engine lane onto its phase column (in_progress → Implementation)", () => {
    render(
      <KanbanBoard
        items={[item({ id: "b", state: "in_progress", blockedReason: "needs_approval" })]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    const lane = screen.getByTestId("column-implementation");
    expect(within(lane).getByTestId("card-b")).toBeInTheDocument();
    // blocked is a condition, never a column (§8.6) — the badge rides the card in-lane.
    expect(within(lane).getByTestId("blocked-badge-b")).toBeInTheDocument();
    expect(screen.queryByTestId("column-in_progress")).toBeNull();
  });

  it("surfaces the owning Role chip on a work-phase column", () => {
    render(
      <KanbanBoard
        items={[]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("column-owner-implementation")).toHaveTextContent(
      "Front-End Engineer",
    );
    expect(screen.getByTestId("column-owner-code_review")).toHaveTextContent("Code Reviewer");
    // Un-owned intake/terminal columns carry no owner chip.
    expect(screen.queryByTestId("column-owner-backlog")).toBeNull();
    expect(screen.queryByTestId("column-owner-done")).toBeNull();
  });

  it("a viewer gets NO drag and NO quick-move control (UI RBAC gate)", () => {
    render(
      <KanbanBoard
        items={[item({ id: "v", state: "todo" })]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("card-v").getAttribute("draggable")).toBe("false");
    expect(screen.queryByTestId("quick-move-v")).toBeNull();
  });

  it("a contributor gets the quick-move control and a draggable card", () => {
    render(
      <KanbanBoard
        items={[item({ id: "c", state: "todo" })]}
        tree={controller({})}
        role="contributor"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("card-c").getAttribute("draggable")).toBe("true");
    expect(screen.getByTestId("quick-move-c")).toBeInTheDocument();
  });

  it("the quick-move menu is guard-limited to allowed targets (no self, no disallowed jump)", () => {
    render(
      <KanbanBoard
        items={[item({ id: "c", state: "implementation" })]}
        tree={controller({})}
        role="contributor"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    const select = screen.getByTestId("quick-move-c") as HTMLSelectElement;
    const values = Array.from(select.options)
      .map((o) => o.value)
      .filter(Boolean);
    // implementation → code_review, testing, planning, design, cancelled (ISI-4455 §2)
    expect(values).toEqual(["code_review", "testing", "planning", "design", "cancelled"]);
    expect(values).not.toContain("implementation"); // never a no-op move
    expect(values).not.toContain("done"); // a disallowed long jump is not offered
  });

  it("a disallowed drop fires NO transition (fail-closed), an allowed one fires exactly one", () => {
    const onTransition = vi.fn(async (_item: WorkItem, _to: PhaseStatus) => {});
    render(
      <KanbanBoard
        items={[item({ id: "c", state: "backlog" })]}
        tree={controller({})}
        role="contributor"
        onTransition={onTransition}
        projectId="ns/demo"
      />,
    );
    const card = screen.getByTestId("card-c");
    // backlog → done is NOT allowed: drop issues no PATCH.
    fireEvent.dragStart(card);
    fireEvent.drop(screen.getByTestId("column-done"));
    expect(onTransition).not.toHaveBeenCalled();
    // backlog → todo IS allowed: exactly one transition.
    fireEvent.dragStart(card);
    fireEvent.drop(screen.getByTestId("column-todo"));
    expect(onTransition).toHaveBeenCalledTimes(1);
    expect(onTransition.mock.calls[0][1]).toBe("todo");
  });

  it("collapsing a super-group hides its columns to a slim rail", () => {
    render(
      <KanbanBoard
        items={[]}
        tree={controller({})}
        role="viewer"
        onTransition={noop}
        projectId="ns/demo"
      />,
    );
    expect(screen.getByTestId("column-design")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("group-toggle-Build"));
    expect(screen.queryByTestId("column-design")).toBeNull();
  });
});
