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

/**
 * Body for the human status-transition (story 8.14a, ADR-037). Field names are
 * the APISERVER's (`internal/apiserver/workitemstate.go` stateTransitionRequest)
 * — the server is the contract authority and the BFF forwards this body
 * verbatim, so a client-side spelling drift surfaces as a 400 on every lane
 * move (ISI-4225). The server treats `fromState` as an optional guard; the
 * console always sends the lane it rendered from so a racing change 409s.
 * Pinned byte-for-byte by the shared contract fixture
 * (test/tickets/fixtures/state-transition-request.json) on both sides.
 *
 * The VALUES are the ISI-4455 ten-status enum (lib/tickets/statusColor
 * `PhaseStatus`), for which the server is the authority — kept as `string` here
 * rather than the legacy 5-value `WorkItemState` so the Kanban can target a
 * phase (e.g. `code_review`) without forking the enum SSOT (ISI-4456 kept the
 * read-model type narrow and folds via statusColor). `fromState` is whatever
 * lane the card rendered from, which may still be a transitional engine state.
 */
export interface StateTransitionBody {
  toState: string;
  fromState: string;
}

/** Create-time priority vocabulary — mirrors the coord enum (validPriorities,
 * pkg/coord/workitemwrite.go) and migration 0020's CHECK. Empty ⇒ no priority. */
export const WORK_ITEM_PRIORITIES = ["low", "medium", "high", "urgent"] as const;
export type WorkItemPriority = (typeof WORK_ITEM_PRIORITIES)[number];

export const PRIORITY_LABELS: Record<WorkItemPriority, string> = {
  low: "Low",
  medium: "Medium",
  high: "High",
  urgent: "Urgent",
};

/** Create-time work-mode vocabulary — mirrors the coord enum (validWorkModes).
 * Only standard|planning are authored (ISI-4409); empty ⇒ default (standard). */
export const WORK_ITEM_MODES = ["standard", "planning"] as const;
export type WorkItemMode = (typeof WORK_ITEM_MODES)[number];

export const WORK_MODE_LABELS: Record<WorkItemMode, string> = {
  standard: "Standard",
  planning: "Planning",
};

/**
 * Body for the human CREATE (S3 / ISI-3959): title is required; `parentId` makes
 * it a sub-issue. State is intentionally absent — a new item lands in the default
 * entry lane; picking a lane is a board move, not a create.
 *
 * priority / workMode / labels are the create-time attributes (ISI-4409): each is
 * optional and only sent when set, so an untouched control is absent (server binds
 * NULL / default), never a fabricated value. The apiserver validates the enums and
 * normalizes labels; the console never posts a value outside the vocabularies above.
 */
export interface CreateWorkItemBody {
  title: string;
  body?: string;
  parentId?: string;
  priority?: WorkItemPriority;
  workMode?: WorkItemMode;
  labels?: string[];
}

/**
 * Body for the human FIELD-EDIT (S3 / ISI-3959): every editable field is optional
 * so an absent field is "leave unchanged". State is NOT editable here (lane moves
 * stay on the state path). `expectedUpdatedAt` is the optimistic-concurrency guard
 * — send the `updatedAt` last read so a racing edit 409s instead of clobbering.
 */
export interface UpdateWorkItemBody {
  title?: string;
  body?: string;
  parentId?: string;
  expectedUpdatedAt?: string;
}
