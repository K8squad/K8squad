// Composer payload builder — the server-stamp boundary (Story 10.3 AC3 /
// §7.3.1). The console sends ONLY `{ body, parentId? }`. Provenance
// (author_*) is stamped server-side from the authenticated principal
// (`internal/discussion/auth.go` PrincipalFromContext); a console that sends
// any `author` field is a defect. This module is the single choke point for
// building an outbound post body so that invariant is enforced in one place
// and can be asserted by test.

/** Everything the composer is allowed to collect from the human. */
export interface ComposerInput {
  body: string;
  /** Set only when replying in-thread; omitted for a new top-level message. */
  parentId?: string | null;
}

/** The exact, minimal wire shape POSTed to the 10.1 message endpoint. */
export interface PostMessageBody {
  body: string;
  parentId?: string;
}

/**
 * Build the outbound POST body. The result contains `body` and — only for a
 * reply — `parentId`. It NEVER contains `author`, `authorId`, `authorType`,
 * `authorName`, `author_agent_id`, or `author_run_id`: provenance is
 * server-stamped, not client-supplied.
 */
export function buildPostBody(input: ComposerInput): PostMessageBody {
  const body = input.body.trim();
  const out: PostMessageBody = { body };
  const parentId = input.parentId?.trim();
  if (parentId) out.parentId = parentId;
  return out;
}

/** True when a composer input is submittable (non-empty after trim). */
export function canSubmit(input: ComposerInput): boolean {
  return input.body.trim().length > 0;
}
