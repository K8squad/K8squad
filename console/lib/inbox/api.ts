// lib/inbox/api.ts — ISI-5535 (E1 of ISI-5531): client-side types + BFF fetch for
// GET /api/squad/inbox (via the Next BFF proxy at /api/inbox). Data via Next BFF only (ADR-013).

/** One row of the Inbox ("Needs Human Decision") aggregate, from GET /api/squad/inbox. */
export type InboxItem = {
  /** Stable union-member key: `inReview:{workItemId}` or `proposal:{messageId}`. */
  key: string;
  /** work_item UUID; empty for create/party_run proposals. */
  ticketId?: string;
  /** Console-addressable project id: "namespace/name". Empty when unresolvable. */
  projectId: string;
  title: string;
  /**
   * Decision chip type — maps to hue:
   * review → violet, proposal → green (approve), choose_one/choose_many → blue, free_form → amber.
   * E2 (ISI-5536) adds more.
   */
  decisionType: "review" | "proposal" | "choose_one" | "choose_many" | "free_form";
  /** Agent name that raised the decision; empty when unknown. */
  raisedByAgent?: string;
  /** ISO-8601 timestamp of the item's last run; null when no run yet. */
  lastRunAt?: string;
  /** True when the item has not been seen since its last run. */
  unread: boolean;
  /** True when the item has a live (active/queued) run (ISI-5528). */
  live?: boolean;
};

export type InboxResponse = {
  /** True for global-admin callers who see the full fleet. */
  fleet?: boolean;
  items: InboxItem[];
};

/** Fetch the inbox via the Next BFF proxy. Throws on network / non-OK response. */
export async function fetchInbox(signal?: AbortSignal): Promise<InboxResponse> {
  const res = await fetch("/api/inbox", { cache: "no-store", signal });
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
    case "proposal":      return "green";
    case "choose_one":
    case "choose_many":   return "blue";
    case "free_form":
    default:              return "amber";
  }
}
