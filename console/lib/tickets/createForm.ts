// lib/tickets/createForm.ts — the single choke point that turns the create-ticket
// form's raw input into the exact wire body the apiserver accepts (ISI-4399 S2,
// design ISI-4231 §2). Kept pure + DOM-free so the validation and body-shaping
// rules are unit-tested without mounting the sheet.
//
// FIDELITY vs. HONESTY (FR-I3): the ISI-4231 mock draws Priority, Work-mode and
// Labels controls alongside Title / Description / Parent. That deferral is now
// LIFTED (ISI-4446): the human CREATE contract (createWorkItemRequest,
// internal/apiserver/workitemwrite.go) accepts { title, body, parentId, priority,
// workMode, labels } since ISI-4409. This builder is still the ONE choke point that
// shapes the wire body, so it emits each optional field ONLY when the user set it —
// an untouched Priority/Work-mode/Labels stays absent (server binds NULL / default),
// never a fabricated value. Assignee stays out: CREATE does not accept it (assignment
// is a post-create / detail concern), so the console never posts it.

import type {
  CreateWorkItemBody,
  WorkItemMode,
  WorkItemPriority,
} from "./types";

/** Raw, still-editable form state as the sheet holds it. */
export interface CreateTicketInput {
  title: string;
  /** Markdown description; the console mirrors markdown, never a rich-text blob. */
  body: string;
  /** Parent work-item id ⇒ the new item is a sub-ticket (adjacency list, §6.1). */
  parentId: string | null;
  /** "" ⇒ no priority chosen (server binds NULL); else one of the coord enum. */
  priority: WorkItemPriority | "";
  /** "" ⇒ leave the server default (standard); else standard|planning. */
  workMode: WorkItemMode | "";
  /** Raw comma/newline-separated labels text as typed; parsed at build time. */
  labels: string;
}

export const EMPTY_CREATE_TICKET: CreateTicketInput = {
  title: "",
  body: "",
  parentId: null,
  priority: "",
  workMode: "",
  labels: "",
};

/**
 * Split the freeform labels field into trimmed, de-duped chips. Accepts comma OR
 * newline separators (chips v1, O-4), drops empties, and preserves first-seen order
 * so the wire order is stable. The coord normalizer trims/de-dups/bounds again on
 * the server — this is the client-side mirror so the form doesn't post obvious junk.
 */
export function parseLabels(raw: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of raw.split(/[,\n]/)) {
    const l = part.trim();
    if (!l || seen.has(l)) continue;
    seen.add(l);
    out.push(l);
  }
  return out;
}

/** Title is the one required field (server rejects an empty title with 400). */
export function canCreate(input: CreateTicketInput): boolean {
  return input.title.trim().length > 0;
}

/**
 * Shape the wire body: trim the title, include every optional field only when the
 * user set it so an untouched control is absent (not a present-empty clear). Mirrors
 * the discussion composer's buildPostBody discipline — the ONLY place the create
 * payload is assembled, so a drift can't leak from the UI. Title stays the one
 * required field; labels are only carried when at least one non-blank chip parses.
 */
export function buildCreateBody(input: CreateTicketInput): CreateWorkItemBody {
  const out: CreateWorkItemBody = { title: input.title.trim() };
  const body = input.body.trim();
  if (body) out.body = body;
  if (input.parentId) out.parentId = input.parentId;
  if (input.priority) out.priority = input.priority;
  if (input.workMode) out.workMode = input.workMode;
  const labels = parseLabels(input.labels);
  if (labels.length > 0) out.labels = labels;
  return out;
}
