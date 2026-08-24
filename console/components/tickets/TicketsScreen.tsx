"use client";

// components/tickets/TicketsScreen.tsx — the Project → Tickets screen (8.14b–d).
//
// Owns everything cross-cutting so both views stay in lockstep:
//   • the view toggle (Kanban ⇄ List) persists per user — localStorage + `?view=`
//     URL param — and a shared search + contextual filters (priority / assignee /
//     label) narrow BOTH views identically (8.14d). Filter/toggle state is
//     read/organization preference, NEVER coord state (R6 scope guard).
//   • the sub-ticket tree's client-only state: lazy children cache + expanded
//     set (8.17) — expansion adds no mutation.
//   • the ONE mutation path: PATCH /work-items/{id}/state {to, expectedFrom}
//     (8.14a). On 409 (or any failure) the screen RE-SYNCS to server truth —
//     it never keeps client-authored state. No claim/lease call exists here.
//   • the caller's role for the UI RBAC gate, resolved via the BFF and
//     FAIL-CLOSED to viewer (§12.3 deny-by-default) — the server wall stays
//     authoritative regardless.
//
// Honest states (FR-I3 / 8.8a discipline): loading, backend-not-yet-available
// (the read model answers a documented 501 until ISI-2909 hosts it), and a
// neutral no-match empty state — never fabricated rows.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ApiError,
  fetchViewerRole,
  listWorkItems,
  patchWorkItemState,
} from "@/lib/tickets/api";
import {
  applyFilters,
  distinctLabels,
  distinctValues,
  filtersToParams,
  isRoot,
} from "@/lib/tickets/derivation";
import {
  EMPTY_FILTERS,
  STATE_LABELS,
  type SortSpec,
  type TicketFilters,
  type WorkItem,
  type WorkItemState,
} from "@/lib/tickets/types";
import {
  persistView,
  resolveInitialView,
  type TicketsView,
} from "@/lib/tickets/viewState";
import { useTreeKeyboardNav, type TreeController } from "./SubTicketTree";
import { KanbanBoard } from "./KanbanBoard";
import { ListView } from "./ListView";
import "./tickets.css";

const EXPANDED_STORAGE_KEY = "ksq.tickets.expanded";

export function TicketsScreen({ projectId }: { projectId: string }) {
  const [role, setRole] = useState<string>("viewer"); // fail-closed until proven otherwise
  const [items, setItems] = useState<WorkItem[]>([]);
  const [childrenCache, setChildrenCache] = useState<Record<string, WorkItem[]>>({});
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const [view, setView] = useState<TicketsView>("kanban");
  const [filters, setFilters] = useState<TicketFilters>(EMPTY_FILTERS);
  const [loading, setLoading] = useState(true);
  const [unavailable, setUnavailable] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [sort, setSort] = useState<SortSpec>({ key: "updated", dir: "desc" });
  const [reloadKey, setReloadKey] = useState(0);
  const filtersRef = useRef(filters);
  filtersRef.current = filters;

  // Initial view: ?view= param first, then localStorage (8.14d AC3). Restore the
  // tree's client-only expansion state the same way (8.17 AC2).
  useEffect(() => {
    setView(resolveInitialView(window.location.search, window.localStorage));
    try {
      const raw = window.localStorage.getItem(EXPANDED_STORAGE_KEY);
      if (raw) setExpanded(new Set(JSON.parse(raw) as string[]));
    } catch {
      /* best-effort view state only */
    }
    void fetchViewerRole().then(setRole);
  }, []);

  const reload = useCallback(async () => {
    setLoading(true);
    setUnavailable(false);
    try {
      // Server-side, tenancy-scoped predicates ride every read (8.14d AC1 /
      // §12.1); the client applies the SAME filters again so Kanban and List
      // can never disagree even if the upstream ignores a param. filtersRef
      // keeps the read current without refetching per keystroke.
      const qs = filtersToParams(filtersRef.current, { sort: "updated_desc" });
      const loaded = await listWorkItems(projectId, { query: qs });
      setItems(loaded.filter(isRoot));
    } catch {
      setUnavailable(true); // documented not-yet-hosted read model — honest empty state
      setItems([]);
    } finally {
      setLoading(false);
    }
  }, [projectId]);

  // `reloadKey` bumps (via onTransition's 409/error path) force a re-sync to
  // server truth without threading it through `reload`'s identity.
  useEffect(() => {
    void reload();
  }, [reload, reloadKey]);

  const persistExpanded = useCallback((next: ReadonlySet<string>) => {
    try {
      window.localStorage.setItem(EXPANDED_STORAGE_KEY, JSON.stringify([...next]));
    } catch {
      /* best-effort view state only */
    }
  }, []);

  const loadChildren = useCallback(
    async (parentId: string): Promise<WorkItem[]> => {
      const kids = await listWorkItems(projectId, { parentId });
      setChildrenCache((prev) => ({ ...prev, [parentId]: kids }));
      return kids;
    },
    [projectId],
  );

  const toggle = useCallback(
    (parentId: string) => {
      setExpanded((prev) => {
        const next = new Set(prev);
        if (next.has(parentId)) next.delete(parentId);
        else {
          if (childrenCache[parentId] == null) void loadChildren(parentId);
          next.add(parentId);
        }
        persistExpanded(next);
        return next;
      });
    },
    [childrenCache, loadChildren, persistExpanded],
  );

  const tree: TreeController = useMemo(
    () => ({
      childrenOf: (parentId) => childrenCache[parentId],
      childCountOf: (item) => item.childCount ?? childrenCache[item.id]?.length ?? 0,
      isExpanded: (parentId) => expanded.has(parentId),
      toggle,
    }),
    [childrenCache, expanded, toggle],
  );

  const treeKeyDown = useTreeKeyboardNav();

  /** 200 ⇒ the card lands in the target column; 409/any failure ⇒ re-sync to server truth. */
  const onTransition = useCallback(
    async (item: WorkItem, to: WorkItemState) => {
      try {
        await patchWorkItemState(item.id, { to, expectedFrom: item.state });
        setNotice(null);
        setItems((prev) =>
          prev.map((it) => (it.id === item.id ? { ...it, state: to } : it)),
        );
        setChildrenCache((prev) => {
          const next: Record<string, WorkItem[]> = {};
          for (const [pid, kids] of Object.entries(prev)) {
            next[pid] = kids.map((it) => (it.id === item.id ? { ...it, state: to } : it));
          }
          return next;
        });
      } catch (err) {
        setNotice(
          err instanceof ApiError && err.status === 409
            ? "Ticket changed under you (409) — re-synced to server state."
            : "Could not move the ticket — re-synced to server state.",
        );
        setReloadKey((k) => k + 1);
      }
    },
    [],
  );

  function switchView(next: TicketsView) {
    setView(next);
    persistView(next, window.localStorage);
    const url = new URL(window.location.href);
    url.searchParams.set("view", next);
    window.history.replaceState(null, "", url); // shareable deep-link, no RSC refetch
  }

  const visible = useMemo(() => applyFilters(items, filters), [items, filters]);
  const priorities = useMemo(() => distinctValues(items, "priority"), [items]);
  const assignees = useMemo(() => distinctValues(items, "assignee"), [items]);
  const labels = useMemo(() => distinctLabels(items), [items]);

  return (
    <div
      className="ksq-tickets"
      data-testid="tickets-screen"
      onKeyDown={treeKeyDown}
    >
      <header className="ksq-tickets__head">
        <h1>Tickets</h1>
        <div className="ksq-viewtoggle" role="group" aria-label="View">
          <button
            type="button"
            className={view === "kanban" ? "is-active" : ""}
            aria-pressed={view === "kanban"}
            data-testid="view-toggle-kanban"
            onClick={() => switchView("kanban")}
          >
            Kanban
          </button>
          <button
            type="button"
            className={view === "list" ? "is-active" : ""}
            aria-pressed={view === "list"}
            data-testid="view-toggle-list"
            onClick={() => switchView("list")}
          >
            List
          </button>
        </div>
      </header>

      <div className="ksq-tickets__filters">
        <input
          type="search"
          placeholder="Search title or ID…"
          aria-label="Search tickets"
          data-testid="filter-query"
          value={filters.query}
          onChange={(evt) => setFilters((f) => ({ ...f, query: evt.target.value }))}
        />
        <select
          aria-label="Filter by priority"
          data-testid="filter-priority"
          value={filters.priority}
          onChange={(evt) => setFilters((f) => ({ ...f, priority: evt.target.value }))}
        >
          <option value="">Priority: any</option>
          {priorities.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <select
          aria-label="Filter by assignee"
          data-testid="filter-assignee"
          value={filters.assignee}
          onChange={(evt) => setFilters((f) => ({ ...f, assignee: evt.target.value }))}
        >
          <option value="">Assignee: any</option>
          {assignees.map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>
        <select
          aria-label="Filter by label"
          data-testid="filter-label"
          value={filters.label}
          onChange={(evt) => setFilters((f) => ({ ...f, label: evt.target.value }))}
        >
          <option value="">Label: any</option>
          {labels.map((l) => (
            <option key={l} value={l}>
              {l}
            </option>
          ))}
        </select>
        <span className="ksq-sr-only" data-testid="sr-role">
          Role: {role}
        </span>
      </div>

      {notice && (
        <p className="ksq-notice" data-testid="resync-notice" role="status">
          {notice}
        </p>
      )}

      {loading ? (
        <p className="ksq-empty-hint" data-testid="tickets-loading">
          Loading tickets…
        </p>
      ) : unavailable ? (
        <div className="ksq-empty-state" data-testid="tickets-unavailable">
          <p className="ksq-empty-state__title">Tickets are not available yet.</p>
          <p className="muted">
            The work-items read model is not hosted by the apiserver yet — nothing
            is fabricated here (FR-I3). Once the backend lands, this view fills in
            from the coordination record.
          </p>
        </div>
      ) : visible.length === 0 ? (
        <div className="ksq-empty-state" data-testid="tickets-empty">
          <p className="ksq-empty-state__title">
            {items.length === 0 ? "No tickets in this Project." : "No tickets match."}
          </p>
          <p className="muted">
            {items.length === 0
              ? "Work items scoped to this Project will appear here."
              : "Adjust the search or filters to widen the set."}
          </p>
        </div>
      ) : view === "kanban" ? (
        <KanbanBoard items={visible} tree={tree} role={role} onTransition={onTransition} />
      ) : (
        <ListView items={visible} tree={tree} sort={sort} onSortChange={setSort} />
      )}

      <p className="ksq-sr-only">States: {Object.values(STATE_LABELS).join(", ")}</p>
    </div>
  );
}
