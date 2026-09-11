// lib/tickets/api.ts — browser-side client for the Tickets BFF surface (8.14b–d).
//
// The browser talks ONLY to the Next.js BFF (ADR-013 single choke point); these
// helpers hit the local /api routes which forward the session cookie upstream.
// The ONE mutation on this screen is the human status-transition
// PATCH /api/work-items/{id}/state {to, expectedFrom} (8.14a, ADR-037) — no
// claim/lease call is ever issued from the console (distinct authority path, §6.2).

import { encodeProjectId } from "@/lib/projectId";
import type {
  CreateWorkItemBody,
  StateTransitionBody,
  UpdateWorkItemBody,
  WorkItem,
  WorkItemState,
} from "./types";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
  ) {
    super(`upstream ${status}`);
  }
}

async function jsonOrThrow(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  try {
    return JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
}

/** Fetch a Project's work items (roots by default; `parentId` ⇒ direct children, 8.17 lazy-load). */
export async function listWorkItems(
  projectId: string,
  opts?: { parentId?: string; query?: string },
): Promise<WorkItem[]> {
  const params = new URLSearchParams();
  if (opts?.parentId) params.set("parentId", opts.parentId);
  if (opts?.query) params.append("raw", opts.query); // pre-built server-side query string
  const qs = opts?.query ?? params.toString();
  const url = `/api/projects/${encodeProjectId(projectId)}/work-items${qs ? `?${qs}` : ""}`;
  const res = await fetch(url, { cache: "no-store" });
  const payload = await jsonOrThrow(res);
  // The M1.5 board read model (ISI-4131) answers a BARE JSON array (never
  // null); the 8.14d-era contract wrapped the list in { items }. Accept the
  // array first — the envelope stays as a fallback for older apiservers so a
  // mixed-version deploy degrades to an empty board, never a crash (ISI-4132).
  const items = Array.isArray(payload)
    ? payload
    : (payload as { items?: unknown }).items;
  return Array.isArray(items) ? (items as WorkItem[]) : [];
}

/** Issue the human status-transition. 200 ⇒ moved; 409 ⇒ stale, caller re-syncs. */
export async function patchWorkItemState(
  workItemId: string,
  body: StateTransitionBody,
): Promise<{ state: WorkItemState }> {
  const res = await fetch(
    `/api/work-items/${encodeURIComponent(workItemId)}/state`,
    {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
    },
  );
  await jsonOrThrow(res);
  return { state: body.to };
}

/**
 * Create a work item (S3 / ISI-3959). 201 ⇒ created; the apiserver returns the new
 * item (with its server-assigned id). A viewer/non-member is refused server-side
 * (403/404) — the UI also fail-closed-hides the action, but the wall is the server.
 * `parentId` in the body makes it a sub-issue.
 */
export async function createWorkItem(
  projectId: string,
  body: CreateWorkItemBody,
): Promise<WorkItem> {
  const res = await fetch(
    `/api/projects/${encodeProjectId(projectId)}/work-items`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
    },
  );
  return (await jsonOrThrow(res)) as WorkItem;
}

/**
 * Edit a work item's fields (S3 / ISI-3959). 200 ⇒ persisted; the apiserver returns
 * the re-synced item. 409 (ApiError) ⇒ a concurrent edit changed it since
 * `expectedUpdatedAt`; the caller re-reads server truth rather than clobbering
 * (mirrors patchWorkItemState's discipline). State is NOT editable here.
 */
export async function updateWorkItem(
  workItemId: string,
  patch: UpdateWorkItemBody,
): Promise<WorkItem> {
  const res = await fetch(
    `/api/work-items/${encodeURIComponent(workItemId)}`,
    {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(patch),
      cache: "no-store",
    },
  );
  return (await jsonOrThrow(res)) as WorkItem;
}

/** Resolve the caller's role for the UI RBAC gate; any failure ⇒ viewer (FAIL-CLOSED, §12.3). */
export async function fetchViewerRole(): Promise<string> {
  try {
    const res = await fetch("/api/session", { cache: "no-store" });
    if (!res.ok) return "viewer";
    const payload = (await res.json()) as { role?: string };
    return payload.role ?? "viewer";
  } catch {
    return "viewer";
  }
}
