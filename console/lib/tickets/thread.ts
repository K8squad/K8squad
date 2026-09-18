// lib/tickets/thread.ts — the ticket-detail (S3 / ISI-4399) read model: the
// browser-side client + pure shaping helpers for GET /api/work-items/{id}, which
// the apiserver answers with coord.WorkItemThread (workitemread.go) — the full
// ticket: description, agent-authored comments, the §6.5 status-change history,
// and the agent-reported change refs (commits / PR links).
//
// WIRE CASING: coord.TaskDetail is embedded WITHOUT json tags, so its fields
// marshal PascalCase (Title, Description, State, Comments, ChangeRefs, Holder,
// RunID…) while the nested arrays and StatusHistory carry lowercase tags. That
// mixed shape is ugly and one add-a-tag away from drifting, so normalizeThread
// is the SINGLE tolerant boundary — it reads either casing and hands the rest of
// the UI one clean camelCase shape. Everything downstream consumes NormalizedThread.

import { ApiError } from "./api";
import type { WorkItemState } from "./types";

/** One append-only comment (coord.comment) — author principal + body + time. */
export interface ThreadComment {
  author: string;
  body: string;
  createdAt: string;
}

/** One agent-reported change ref (coord.change_ref) — commit SHA or PR URL. */
export interface ThreadChangeRef {
  kind: string;
  ref: string;
  summary?: string;
  author: string;
  runId?: string;
  createdAt: string;
}

/** One lane move from the §6.5 audit history. */
export interface ThreadStatusChange {
  fromState: string;
  toState: string;
  principal: string;
  occurredAt: string;
}

/** The clean, camelCase ticket-detail shape the UI renders. */
export interface NormalizedThread {
  workItemId: string;
  title: string;
  description: string;
  state: WorkItemState | string;
  blockedReason: string;
  comments: ThreadComment[];
  changeRefs: ThreadChangeRef[];
  statusHistory: ThreadStatusChange[];
  /** Current claim holder principal ("" ⇒ unclaimed) + holding run, for the sidebar. */
  holder: string;
  runId: string;
  /**
   * The human-requested agent (dispatch stamp, ISI-4567 §2.1): set the moment a
   * dispatch/re-assign picks a name, BEFORE any run claims the ticket. null ⇒
   * nobody requested yet — the rail renders the honest placeholder, never a guess.
   */
  requestedAgent: string | null;
  /**
   * The claim assignee (coord.claim's agent attribution of the current/last
   * attempt, taskdetail.go Assignee). null ⇒ no attribution on the wire.
   */
  assignee: string | null;
}

// Casing-tolerant readers — accept the server's PascalCase OR a future camelCase.
function str(raw: Record<string, unknown>, ...keys: string[]): string {
  for (const k of keys) {
    const v = raw[k];
    if (typeof v === "string") return v;
  }
  return "";
}

function arr(raw: Record<string, unknown>, ...keys: string[]): unknown[] {
  for (const k of keys) {
    const v = raw[k];
    if (Array.isArray(v)) return v;
  }
  return [];
}

/** Map the raw wire object (either casing) into the clean shape. */
export function normalizeThread(raw: unknown): NormalizedThread {
  const r = (raw ?? {}) as Record<string, unknown>;
  const comments = arr(r, "Comments", "comments").map((c) => {
    const o = c as Record<string, unknown>;
    return {
      author: str(o, "author", "Author"),
      body: str(o, "body", "Body"),
      createdAt: str(o, "createdAt", "CreatedAt"),
    };
  });
  const changeRefs = arr(r, "ChangeRefs", "changeRefs").map((c) => {
    const o = c as Record<string, unknown>;
    const summary = str(o, "summary", "Summary");
    const runId = str(o, "runId", "RunID");
    return {
      kind: str(o, "kind", "Kind"),
      ref: str(o, "ref", "Ref"),
      ...(summary ? { summary } : {}),
      author: str(o, "author", "Author"),
      ...(runId ? { runId } : {}),
      createdAt: str(o, "createdAt", "CreatedAt"),
    };
  });
  const statusHistory = arr(r, "statusHistory", "StatusHistory").map((c) => {
    const o = c as Record<string, unknown>;
    return {
      fromState: str(o, "fromState", "FromState"),
      toState: str(o, "toState", "ToState"),
      principal: str(o, "principal", "Principal"),
      occurredAt: str(o, "occurredAt", "OccurredAt"),
    };
  });
  return {
    workItemId: str(r, "WorkItemID", "workItemId", "id"),
    title: str(r, "Title", "title"),
    description: str(r, "Description", "description", "body"),
    state: str(r, "State", "state"),
    blockedReason: str(r, "BlockedReason", "blockedReason"),
    comments,
    changeRefs,
    statusHistory,
    holder: str(r, "Holder", "holder"),
    runId: str(r, "RunID", "runId"),
    requestedAgent: str(r, "RequestedAgent", "requestedAgent") || null,
    assignee: str(r, "Assignee", "assignee") || null,
  };
}

/** Fetch + normalize one ticket's thread via the BFF (throws ApiError on non-2xx). */
export async function fetchWorkItemThread(
  workItemId: string,
): Promise<NormalizedThread> {
  const res = await fetch(`/api/work-items/${encodeURIComponent(workItemId)}`, {
    cache: "no-store",
  });
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
  return normalizeThread(raw);
}

/**
 * Append a HUMAN comment to a ticket's thread via the BFF
 * (POST /api/work-items/{id}/comments, ISI-4406 endpoint / ISI-4454 composer).
 * The body carries only { body } text — authorship is server-stamped from the
 * session principal, never trusted from the client. On 201 the apiserver returns
 * the persisted comment in the SAME shape the thread read emits
 * ({ author, body, createdAt }), so the composer can append it optimistically
 * before the reconciling re-fetch lands. Non-2xx throws ApiError (status + body):
 * the composer surfaces 400/401/403 inline and treats 404/501 as the honest
 * "endpoint not wired on this deployment" gap (FR-I3).
 *
 * ISI-4495: the 201 body ALSO carries the comment-triggered re-dispatch outcome
 * (reTriggered/fromState/toState) when the comment nudged a parked ticket back
 * to the dispatch lane — additive fields an older apiserver simply omits, so a
 * mixed-version deploy degrades to "no feedback line", never a crash.
 */
export interface PostedComment extends ThreadComment {
  /** True when this comment re-dispatched the ticket (parked → todo, ISI-4495). */
  reTriggered?: boolean;
  fromState?: string;
  toState?: string;
}

export async function postWorkItemComment(
  workItemId: string,
  body: string,
): Promise<PostedComment> {
  const res = await fetch(
    `/api/work-items/${encodeURIComponent(workItemId)}/comments`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ body }),
      cache: "no-store",
    },
  );
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
  const o = (raw ?? {}) as Record<string, unknown>;
  return {
    author: str(o, "author", "Author"),
    body: str(o, "body", "Body"),
    createdAt: str(o, "createdAt", "CreatedAt"),
    reTriggered: o.reTriggered === true || o.ReTriggered === true || undefined,
    fromState: str(o, "fromState", "FromState") || undefined,
    toState: str(o, "toState", "ToState") || undefined,
  };
}

// ---------------------------------------------------------------------------
// Pure activity-timeline shaping (unit-tested — no fetch, no DOM).
// ---------------------------------------------------------------------------

export type ActivityKind = "comment" | "event" | "change";

/** One unified row in the chronological Activity thread (design §3). */
export interface ActivityItem {
  kind: ActivityKind;
  at: string;
  /** author/principal for comment & change; "" for a bare event. */
  who: string;
  /** the human-role of `who` — a comment/change from an agent vs a person. */
  authorKind: "agent" | "user";
  comment?: ThreadComment;
  change?: ThreadChangeRef;
  event?: ThreadStatusChange;
}

/** A principal like "agent:builder" is an agent; anything else is a person. */
export function authorKind(principal: string): "agent" | "user" {
  return principal.startsWith("agent:") || principal.startsWith("agent/")
    ? "agent"
    : "user";
}

/**
 * Merge comments + status changes + change refs into ONE chronological list,
 * NEWEST LAST (design §3 "chronological, newest last"). A missing/unparseable
 * timestamp sorts to the end (0 epoch would wrongly float to the top of an
 * ascending sort, so treat it as +∞ instead — an undated row is "just now").
 */
export function buildActivity(thread: NormalizedThread): ActivityItem[] {
  const items: ActivityItem[] = [];
  for (const c of thread.comments) {
    items.push({
      kind: "comment",
      at: c.createdAt,
      who: c.author,
      authorKind: authorKind(c.author),
      comment: c,
    });
  }
  for (const e of thread.statusHistory) {
    items.push({
      kind: "event",
      at: e.occurredAt,
      who: e.principal,
      authorKind: authorKind(e.principal),
      event: e,
    });
  }
  for (const c of thread.changeRefs) {
    items.push({
      kind: "change",
      at: c.createdAt,
      who: c.author,
      authorKind: authorKind(c.author),
      change: c,
    });
  }
  const at = (s: string) => {
    const t = Date.parse(s);
    return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
  };
  return items.sort((a, b) => at(a.at) - at(b.at));
}

/** Sub-ticket rollup for the "N of M done" progress line (design §3). */
export function subTicketProgress(children: { state: string }[]): {
  done: number;
  total: number;
} {
  let done = 0;
  for (const c of children) if (c.state === "done") done += 1;
  return { done, total: children.length };
}

/**
 * Sub-ticket status roll-up for the ISI-4447 redesign's right-rail "Sub-tickets
 * status" card: the three buckets the count tiles + progress bar render (done /
 * in-progress / todo) plus the total. The board's five states collapse to the
 * card's three columns the same way the Kanban lanes group for a summary —
 * `in_review` counts as in-progress work (it is not yet done, not backlog), and
 * `backlog`+`todo` both read as "todo" (not-yet-started). Pure + DOM-free so the
 * bucketing rule is unit-tested without mounting the card.
 */
export function subTicketStatus(children: { state: string }[]): {
  done: number;
  inProgress: number;
  todo: number;
  total: number;
} {
  let done = 0;
  let inProgress = 0;
  let todo = 0;
  for (const c of children) {
    if (c.state === "done") done += 1;
    else if (c.state === "in_progress" || c.state === "in_review") inProgress += 1;
    else todo += 1; // backlog + todo == not-yet-started
  }
  return { done, inProgress, todo, total: children.length };
}
