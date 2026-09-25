// Composer payload builder — the server-stamp boundary (Story 10.3 AC3 /
// §7.3.1). The console sends ONLY `{ body, parentId?, audience? }`. Provenance
// (author_*) is stamped server-side from the authenticated principal
// (`internal/discussion/auth.go` PrincipalFromContext); a console that sends
// any `author` field is a defect. This module is the single choke point for
// building an outbound post body so that invariant is enforced in one place
// and can be asserted by test.

import { audienceWire, type AudienceLike } from "./audience";

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
}

/** The exact, minimal wire shape POSTed to the 10.1 message endpoint. */
export interface PostMessageBody {
  body: string;
  parentId?: string;
  /** Present only for a direct post — the server defaults omitted to `party`. */
  audience?: string;
}

/**
 * Build the outbound POST body. The result contains `body` and — only for a
 * reply — `parentId`, and — only for a direct audience — `audience`. It NEVER
 * contains `author`, `authorId`, `authorType`, `authorName`, `author_agent_id`,
 * or `author_run_id`: provenance is server-stamped, not client-supplied.
 */
export function buildPostBody(input: ComposerInput): PostMessageBody {
  const body = input.body.trim();
  const out: PostMessageBody = { body };
  const parentId = input.parentId?.trim();
  if (parentId) out.parentId = parentId;
  const audience = audienceWire(input.audience);
  if (audience) out.audience = audience;
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
