// lib/tickets/api.ts — browser-side client for the Tickets BFF surface (8.14b–d).
//
// The browser talks ONLY to the Next.js BFF (ADR-013 single choke point); these
// helpers hit the local /api routes which forward the session cookie upstream.
// The ONE mutation on this screen is the human status-transition
// PATCH /api/work-items/{id}/state {toState, fromState} (8.14a, ADR-037 — the
// apiserver's field names, ISI-4225) — no claim/lease call is ever issued from
// the console (distinct authority path, §6.2).

import { encodeProjectId } from "@/lib/projectId";
import type { GithubIssue } from "@/lib/github-status";
import type {
  CreateWorkItemBody,
  StateTransitionBody,
  UpdateWorkItemBody,
  WorkItem,
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
): Promise<{ state: string }> {
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
  return { state: body.toState };
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

/** One assignable agent for the create-sheet dropdown (ISI-4501). */
export interface AgentOption {
  /** Object UID — React key only; NOT what dispatch matches on. */
  id: string;
  /** Agent NAME — the identity `POST .../dispatch {agentId}` resolves against
   * (coord.RequestDispatch checks Team.Spec.Agents names, fleetlist.go). */
  name: string;
  /** Referenced Role name (fleetlist.go AgentListEntry.Role), shown as a
   * "role — name" label so a human picks by function not opaque handle
   * (ISI-4807 §2). Empty when the agent declares no role. */
  role?: string;
}

/** The dropdown label for an assignable agent: "role — name" when the agent
 * carries a role, else the bare name (ISI-4807 §2). Kept pure + exported so the
 * create-sheet and the detail rail render one identical label. */
export function agentOptionLabel(a: AgentOption): string {
  return a.role ? `${a.role} — ${a.name}` : a.name;
}

/**
 * List the squad's agents for the "Assign to" dropdown (ISI-4501). Proxies
 * GET /api/squad/agents (fleetlist.go FleetAgentList → {agents:[{id,name}]}),
 * project-scoped server-side. BEST-EFFORT: any failure (unhosted 501, 404,
 * network) degrades to an empty roster so the sheet still creates unassigned —
 * the dropdown just shows "Unassigned" alone rather than blocking create.
 */
export async function listSquadAgents(): Promise<AgentOption[]> {
  try {
    const res = await fetch("/api/squad/agents", {
      headers: { accept: "application/json" },
      cache: "no-store",
    });
    if (!res.ok) return [];
    const payload = (await res.json()) as { agents?: unknown };
    const rows = Array.isArray(payload.agents) ? payload.agents : [];
    const mapped = rows
      .map((r) => r as { id?: unknown; name?: unknown; role?: unknown })
      .filter((r) => typeof r.id === "string" && typeof r.name === "string")
      .map((r) => ({
        id: r.id as string,
        name: r.name as string,
        role: typeof r.role === "string" ? (r.role as string) : undefined,
      }));
    // Dedupe by agent NAME (ISI-4807 §1): a fleet/admin caller's GET /api/squad/agents
    // spans every squad namespace (fleetlist.go scope), so an agent present in two
    // namespaces (e.g. k8squad-system + k8squad-preview) returns twice — "2× the
    // agents". Dispatch matches on name against the owning Team's composition, which
    // is name-unique, so collapsing on name is correct: the first occurrence wins and
    // the picker shows each agent exactly once.
    const seen = new Set<string>();
    const deduped: AgentOption[] = [];
    for (const a of mapped) {
      if (seen.has(a.name)) continue;
      seen.add(a.name);
      deduped.push(a);
    }
    return deduped;
  } catch {
    return [];
  }
}

/** Result of a board dispatch (workitemdispatch.go WorkItemDispatchResult). */
export interface DispatchResult {
  workItemId: string;
  fromState: string;
  toState: string;
  requestedAgent: string;
}

/**
 * Dispatch a human's agent choice for a work item (ISI-4501, ADR-0022 / ISI-4411):
 * POST /api/work-items/{id}/dispatch {agentId}. This is NOT a create-body field —
 * assignment is custody/dispatch-based: it stamps the requested agent and advances
 * backlog→todo so operator Intake mints the Run. `agentId` is the agent NAME (the
 * identity Team.Spec.Agents and Intake dispatch on). 200 ⇒ dispatched; 403 (agent
 * not in team / human-only), 404 (item gone), 409 (item not in backlog) surface
 * verbatim as ApiError so the sheet can report the assignment failed without
 * pretending the create failed too.
 */
export async function dispatchWorkItem(
  workItemId: string,
  agentId: string,
): Promise<DispatchResult> {
  const res = await fetch(
    `/api/work-items/${encodeURIComponent(workItemId)}/dispatch`,
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ agentId }),
      cache: "no-store",
    },
  );
  return (await jsonOrThrow(res)) as DispatchResult;
}

/** The Epic-2 bridge result (ISI-4757 contract): the apiserver finds-or-creates the
 * work-item for a GitHub issue and dispatches the chosen agent atomically. `created`
 * is false when an existing work-item was reused (idempotent re-assign). */
export interface GithubAssignResult {
  workItemId: string;
  /** The compact issue ref `owner/repo#number` the apiserver derived + labelled. */
  issueRef: string;
  created: boolean;
  dispatch: { requestedAgent: string; fromState: string; toState: string };
}

/** Discriminated failure code for the assign-&-dispatch call, mapped from the
 * Epic-2 status codes so the UI can surface the right §6 message. */
export type GithubAssignErrorCode =
  | "invalid-agent" // 400 — invalid/missing agent
  | "forbidden" // 403 — human-only / agent not in this Team
  | "already" // 409 — already dispatched / bridged (non-destructive)
  | "not-found" // 404 — issue not resolvable
  | "not-hosted" // 501 — bridge seam not hosted in this deployment
  | "failed"; // network / other

export type GithubAssignOutcome =
  | { ok: true; result: GithubAssignResult }
  | { ok: false; status: number; code: GithubAssignErrorCode };

function mapGithubAssignError(status: number): GithubAssignErrorCode {
  switch (status) {
    case 400:
      return "invalid-agent";
    case 403:
      return "forbidden";
    case 409:
      return "already";
    case 404:
      return "not-found";
    case 501:
      return "not-hosted";
    default:
      return "failed";
  }
}

/**
 * Assign a GitHub issue to one of the squad's agents (ISI-4759 / ISI-4749 epic 3),
 * calling the Epic-2 bridge (ISI-4757) via its BFF proxy:
 *   POST /api/projects/{projectId}/github/issues/{number}/assign {agentId, url}
 * The apiserver does find-or-create work-item + dispatch in ONE atomic call,
 * keyed on the dedicated idempotency label `ksquad.github.issue=owner/repo#N`
 * (NOT the telemetry join-key), so a repeat click is safe (created:false / 409).
 * `agentId` carries the agent NAME (ISI-4501 dispatch convention). Honesty
 * (ADR-0013): this is a Paperclip-side dispatch only — it NEVER writes GitHub
 * assignees. Returns a discriminated outcome so the popup maps states without
 * throwing.
 */
export async function assignAndDispatch(
  projectId: string,
  issue: Pick<GithubIssue, "number" | "url">,
  agentName: string,
): Promise<GithubAssignOutcome> {
  try {
    const res = await fetch(
      `/api/projects/${encodeProjectId(projectId)}/github/issues/${issue.number}/assign`,
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ agentId: agentName, url: issue.url }),
        cache: "no-store",
      },
    );
    if (res.ok) {
      return { ok: true, result: (await res.json()) as GithubAssignResult };
    }
    return { ok: false, status: res.status, code: mapGithubAssignError(res.status) };
  } catch {
    return { ok: false, status: 0, code: "failed" };
  }
}

/**
 * Resolve the caller's role for the UI RBAC gate; any failure ⇒ "viewer" (FAIL-CLOSED, §12.3).
 *
 * GET /api/session proxies /auth/me VERBATIM (see app/api/session/route.ts). That body is a
 * publicUser whose role lives in `globalRole` ("admin" | "user") — NOT `role`, and NOT the
 * per-Project vocab (viewer/contributor/maintainer), which is a route-scoped, existence-hidden
 * axis /auth/me never exposes (mirrors lib/session.ts viewer(), which already reads globalRole).
 * We therefore read `globalRole`: any signed-in caller clears the create/comment gate (the same
 * doctrine as canCompose — offer the action, let the server wall be authoritative), and only an
 * unresolved caller (no session / non-200 / field absent) fails closed to the "viewer" sentinel.
 *
 * ISI-4496: the prior read of `payload.role` matched no field, so EVERY caller pinned to "viewer"
 * and the "+ New issue" button vanished for admins too — indistinguishable from "not shipped".
 */
export async function fetchViewerRole(): Promise<string> {
  try {
    const res = await fetch("/api/session", { cache: "no-store" });
    if (!res.ok) return "viewer";
    const payload = (await res.json()) as { globalRole?: string };
    return payload.globalRole ?? "viewer";
  } catch {
    return "viewer";
  }
}
