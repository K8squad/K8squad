// lib/tickets/createForm.ts — the single choke point that turns the create-ticket
// form's raw input into the exact wire body the apiserver accepts (ISI-4399 S2,
// design ISI-4231 §2). Kept pure + DOM-free so the validation and body-shaping
// rules are unit-tested without mounting the sheet.
//
// FIDELITY vs. HONESTY (FR-I3): the ISI-4231 mock also draws Work-mode, Priority,
// Assignee and Labels controls, but the human CREATE contract
// (createWorkItemRequest, internal/apiserver/workitemwrite.go) accepts ONLY
// { title, body, parentId } today. The console never posts fields the server would
// silently drop, so this builder emits exactly that trio — the richer controls are
// gated on a create-API extension tracked as a separate backend story, not faked
// here.

import type { CreateWorkItemBody } from "./types";

/** Raw, still-editable form state as the sheet holds it. */
export interface CreateTicketInput {
  title: string;
  /** Markdown description; the console mirrors markdown, never a rich-text blob. */
  body: string;
  /** Parent work-item id ⇒ the new item is a sub-ticket (adjacency list, §6.1). */
  parentId: string | null;
}

export const EMPTY_CREATE_TICKET: CreateTicketInput = {
  title: "",
  body: "",
  parentId: null,
};

/** Title is the one required field (server rejects an empty title with 400). */
export function canCreate(input: CreateTicketInput): boolean {
  return input.title.trim().length > 0;
}

/**
 * Shape the wire body: trim the title, include body/parentId only when non-empty
 * so an untouched optional field is absent (not a present-empty clear). Mirrors
 * the discussion composer's buildPostBody discipline — the ONLY place the create
 * payload is assembled, so a drift can't leak from the UI.
 */
export function buildCreateBody(input: CreateTicketInput): CreateWorkItemBody {
  const out: CreateWorkItemBody = { title: input.title.trim() };
  const body = input.body.trim();
  if (body) out.body = body;
  if (input.parentId) out.parentId = input.parentId;
  return out;
}
