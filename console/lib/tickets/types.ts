// lib/tickets/types.ts — shared types for the Project → Tickets surface
// (stories 8.14b–d + 8.17).
//
// The shape mirrors coord.work_item (db/migrations/0001_coord_schema.sql §6.1):
// state is the canonical five-value board enum (§13 board-derivation — the Kanban
// board is a PROJECTION of `state`, never a stored column), `blocked_reason` is an
// orthogonal condition (§8.6 — never a lane), and `parent_id` is the adjacency
// list behind the 8.17 sub-ticket tree.
//
// priority / assignee / labels are OPTIONAL: the console renders whatever the
// read model returns and shows an honest empty state ("—") when a field is absent
// — never a fabricated value (FR-I3 provenance, same discipline as story 8.8).

/** Canonical ordered board states (§6.1 CHECK constraint, §13 derivation order). */
export const WORK_ITEM_STATES = [
  "backlog",
  "todo",
  "in_progress",
  "in_review",
  "done",
] as const;

export type WorkItemState = (typeof WORK_ITEM_STATES)[number];

export const STATE_LABELS: Record<WorkItemState, string> = {
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In Progress",
  in_review: "In Review",
  done: "Done",
};

/** Caller role for the UI RBAC gate mirroring the §6.7.2/§12.3 server wall. */
export type ViewerRole = "viewer" | "contributor" | "maintainer" | "admin";

/** A work item as returned by the BFF read (JSON field names match the apiserver). */
export interface WorkItem {
  id: string;
  projectId: string;
  parentId: string | null;
  title: string;
  state: WorkItemState;
  /** NULL ⇒ not blocked; a blocked item stays in its lane with a badge overlay (§8.6). */
  blockedReason: string | null;
  /** Optional per FR-I3 — render "—", never invent. */
  priority?: string | null;
  assignee?: string | null;
  labels?: string[] | null;
  updatedAt: string;
  /** Direct-child count for the 8.17 caret + badge; undefined ⇒ unknown until loaded. */
  childCount?: number;
  /** SCM-synced provenance badge (§5.4, Epic 11) — e.g. "github". */
  provenance?: string | null;
}

/** Contextual filters shared identically by both views (story 8.14d). */
export interface TicketFilters {
  /** Free text over title / ID (screen-local search, NOT the 8.18 global track). */
  query: string;
  priority: string;
  assignee: string;
  label: string;
}

export const EMPTY_FILTERS: TicketFilters = {
  query: "",
  priority: "",
  assignee: "",
  label: "",
};

export type SortKey =
  | "id"
  | "title"
  | "status"
  | "priority"
  | "assignee"
  | "labels"
  | "updated";

export type SortDir = "asc" | "desc";

export interface SortSpec {
  key: SortKey;
  dir: SortDir;
}

/** Body for the human status-transition (story 8.14a contract, ADR-037). */
export interface StateTransitionBody {
  to: WorkItemState;
  expectedFrom: WorkItemState;
}
