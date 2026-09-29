// Composer payload builder — the server-stamp boundary (Story 10.3 AC3 /
// §7.3.1). The console sends ONLY `{ body, parentId?, audience? }`. Provenance
// (author_*) is stamped server-side from the authenticated principal
// (`internal/discussion/auth.go` PrincipalFromContext); a console that sends
// any `author` field is a defect. This module is the single choke point for
// building an outbound post body so that invariant is enforced in one place
// and can be asserted by test.

import { audienceWire, type AudienceLike } from "./audience";

/**
 * One structured ticket reference the composer's `#` picker collected
 * (ISI-5167 / ISI-5134 S3). It is a LINK, not a dispatch: `workItemId` is the
 * coord work-item UUID the picker carried (its stable key), and `title` is the
 * display title for chip rendering. Field names match the Go JSON tags on
 * `internal/discussion/dispatch.go#TicketRef` so the wire shape lines up with
 * the S1 (ISI-5165) resolver, which validates each ref against the message's
 * project and drops unknown/out-of-project ids server-side.
 */
export interface TicketReference {
  workItemId: string;
  title?: string;
}

/** Everything the composer is allowed to collect from the human. */
export interface ComposerInput {
  body: string;
  /** Set only when replying in-thread; omitted for a new top-level message. */
  parentId?: string | null;
  /**
   * Audience for the post (ISI-4929): `party` (default, board OQ2) or a direct
   * target (`{ kind: "direct", agentId }`, or an already-built `direct:…`
   * token — the builder is idempotent). Provenance is still server-stamped;
   * audience only scopes delivery.
   */
  audience?: AudienceLike;
  /**
   * Ticket LINKS the `#` picker collected (ISI-5167). Absent/empty ⇒ no
   * `references` key on the wire, so a plain message stays link-free. These are
   * links, never dispatch targets: the server resolves them and drops any that
   * do not belong to the room's project (ISI-5165).
   */
  references?: readonly TicketReference[];
}

/** The exact, minimal wire shape POSTed to the 10.1 message endpoint. */
export interface PostMessageBody {
  body: string;
  parentId?: string;
  /** Present only for a direct post — the server defaults omitted to `party`. */
  audience?: string;
  /** Present only when the `#` picker collected at least one ticket link (ISI-5167). */
  references?: TicketReference[];
}

/**
 * De-duplicate and trim ticket references: drop blank ids, collapse duplicate
 * work-item ids (first-seen wins, order preserved), and trim titles. The
 * backend re-validates and re-dedupes, so this only keeps the wire tidy — it is
 * NOT the authority on which links survive.
 */
function normalizeReferences(
  refs?: readonly TicketReference[],
): TicketReference[] {
  if (!refs || refs.length === 0) return [];
  const seen = new Set<string>();
  const out: TicketReference[] = [];
  for (const r of refs) {
    const id = r.workItemId?.trim();
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const title = r.title?.trim();
    out.push(title ? { workItemId: id, title } : { workItemId: id });
  }
  return out;
}

/**
 * Build the outbound POST body. The result contains `body` and — only for a
 * reply — `parentId`, only for a direct audience — `audience`, and only when
 * the `#` picker collected links — `references` (ISI-5167). It NEVER contains
 * `author`, `authorId`, `authorType`, `authorName`, `author_agent_id`, or
 * `author_run_id`: provenance is server-stamped, not client-supplied.
 */
export function buildPostBody(input: ComposerInput): PostMessageBody {
  const body = input.body.trim();
  const out: PostMessageBody = { body };
  const parentId = input.parentId?.trim();
  if (parentId) out.parentId = parentId;
  const audience = audienceWire(input.audience);
  if (audience) out.audience = audience;
  const references = normalizeReferences(input.references);
  if (references.length > 0) out.references = references;
  return out;
}

/** True when a composer input is submittable (non-empty after trim). */
export function canSubmit(input: ComposerInput): boolean {
  return input.body.trim().length > 0;
}

/** Everything the composer collects to OPEN a new thread (AC2). */
export interface OpenThreadInput {
  title: string;
  body: string;
}

/** The exact, minimal wire shape POSTed to the open-thread endpoint. */
export interface OpenThreadBody {
  title: string;
  body: string;
}

/**
 * Build the outbound open-thread POST body. Like {@link buildPostBody} this is
 * the single server-stamp choke point: the result carries ONLY `{ title, body }`
 * and NEVER any `author*`/`createdBy`/`principal` field — thread creator and
 * message provenance are stamped server-side from the authenticated principal.
 */
export function buildOpenThreadBody(input: OpenThreadInput): OpenThreadBody {
  return { title: input.title.trim(), body: input.body.trim() };
}

/** True when an open-thread input is submittable (title AND body non-empty). */
export function canOpenThread(input: OpenThreadInput): boolean {
  return input.title.trim().length > 0 && input.body.trim().length > 0;
}
