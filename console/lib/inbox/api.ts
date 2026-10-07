// lib/inbox/api.ts — ISI-5535 (E1 of ISI-5531): client-side types + BFF fetch for
// GET /api/squad/inbox (via the Next BFF proxy at /api/inbox). Data via Next BFF only (ADR-013).

/** One selectable option on a choose_* decision row — the inline-answer affordance (ISI-5537 E3). */
export type InboxDecisionOption = {
  id: string;
  label: string;
  recommended?: boolean;
};

/**
 * The inline-answer affordance for a decision_request row (ISI-5537 E3). Present only on decision
 * rows (approve / choose_* / free_form). The triage row POSTs the answer/reject to the project-scoped
 * decision endpoints keyed by `messageId` (ADR-0026 §4.4). approve/choose_one answer inline; the
 * other modes (and any reject that requires a reason) route to the ticket-detail card instead.
 */
export type InboxDecision = {
  messageId: string;
  options?: InboxDecisionOption[];
  allowFreeText?: boolean;
  allowReject: boolean;
  rejectRequiresReason?: boolean;
};

/** One row of the Inbox ("Needs Human Decision") aggregate, from GET /api/squad/inbox. */
export type InboxItem = {
  /** Stable union-member key: `inReview:{workItemId}`, `proposal:{messageId}`, `decision:{messageId}`. */
  key: string;
  /** work_item UUID; empty for create/party_run proposals. */
  ticketId?: string;
  /** Console-addressable project id: "namespace/name". Empty when unresolvable. */
  projectId: string;
  title: string;
  /**
   * Decision chip type — maps to hue:
   * review → violet, proposal/approve → green, choose_one/choose_many → blue, free_form → amber.
   */
  decisionType:
    | "review"
    | "proposal"
    | "approve"
    | "choose_one"
    | "choose_many"
    | "free_form";
  /** Agent name that raised the decision; empty when unknown. */
  raisedByAgent?: string;
  /** ISO-8601 timestamp of the item's last run; null when no run yet. */
  lastRunAt?: string;
  /** True when the item has not been seen since its last run. */
  unread: boolean;
  /** True when the item has a live (active/queued) run (ISI-5528). */
  live?: boolean;
  /** Inline-answer affordance; set only on decision_request rows (ISI-5537 E3). */
  decision?: InboxDecision;
};

export type InboxResponse = {
  /** True for global-admin callers who see the full fleet. */
  fleet?: boolean;
  items: InboxItem[];
};

/**
 * Fetch the inbox via the Next BFF proxy. Throws on network / non-OK response.
 * `scope: "mine"` (ISI-5537 E3) narrows an admin's fleet view back to their own team; a no-op for
 * non-admins (already team-fenced).
 */
export async function fetchInbox(
  opts?: { scope?: "mine"; signal?: AbortSignal },
): Promise<InboxResponse> {
  const q = opts?.scope === "mine" ? "?scope=mine" : "";
  const res = await fetch(`/api/inbox${q}`, { cache: "no-store", signal: opts?.signal });
  if (!res.ok) throw new Error(`inbox: ${res.status}`);
  return res.json() as Promise<InboxResponse>;
}

/**
 * Mark the given inbox item keys as seen (ADR-0026 §6) via the Next BFF proxy, so their unread
 * dots and the nav badge clear. Fire-and-forget: best-effort, swallows errors (a failed mark-seen
 * just leaves the items unread — the GET will re-derive truth on the next poll). No-op for [].
 */
export async function inboxMarkSeen(keys: string[]): Promise<void> {
  if (keys.length === 0) return;
  try {
    await fetch("/api/inbox/seen", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ keys }),
      cache: "no-store",
    });
  } catch {
    // best-effort; unread simply persists until the next view
  }
}

/**
 * Mark the given inbox item keys UNREAD (ISI-5537 E3) via the Next BFF proxy — the "mark unread" half
 * of the read/unread toggle. Drops the per-user read-markers so the items re-derive as unread on the
 * next poll. Fire-and-forget, same posture as inboxMarkSeen. No-op for [].
 */
export async function inboxMarkUnread(keys: string[]): Promise<void> {
  if (keys.length === 0) return;
  try {
    await fetch("/api/inbox/unseen", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ keys }),
      cache: "no-store",
    });
  } catch {
    // best-effort; the read-marker simply stays until the next explicit toggle
  }
}

/** Build the click-through href for an inbox item. */
export function inboxItemHref(item: InboxItem): string {
  if (item.ticketId && item.projectId) {
    return `/projects/${encodeURIComponent(item.projectId)}/issues/${item.ticketId}`;
  }
  if (item.projectId) {
    return `/projects/${encodeURIComponent(item.projectId)}/discussion`;
  }
  return "/inbox";
}

/** Map a decisionType to its chip colour class (violet/green/blue/amber). */
export function decisionChipHue(decisionType: string): "violet" | "green" | "blue" | "amber" {
  switch (decisionType) {
    case "review":        return "violet";
    case "proposal":
    case "approve":       return "green";
    case "choose_one":
    case "choose_many":   return "blue";
    case "free_form":
    default:              return "amber";
  }
}
