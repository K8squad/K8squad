// Audience wire helpers (ISI-4929, plan §4.1/§4.2 — Discussion Room v2 story 5).
//
// `audience` is a server-validated message field with exactly two shapes:
//   - `"party"`            — room-visible to everyone (the default, board OQ2)
//   - `"direct:{agentId}"` — scoped to author + recipient (+ admins) server-side
//
// Visibility is enforced in the apiserver Store reads and is NEVER client-trusted;
// these helpers only build/parse the wire token and drive the composer's
// audience selector. The server's `normalizeAudience` (internal/discussion/store.go)
// is the authority: a malformed token (empty target, unknown prefix) is rejected
// there with ErrInvalidAudience.

/** The parsed, in-memory audience of a message or composer state. */
export type Audience = { kind: "party" } | { kind: "direct"; agentId: string };

/** The literal party wire token (also the server default when omitted). */
export const PARTY_AUDIENCE = "party";

/** Build the `direct:{agentId}` wire token. An empty id is a caller defect → party. */
export function directAudience(agentId: string): string {
  const id = agentId.trim();
  return id ? `direct:${id}` : PARTY_AUDIENCE;
}

/**
 * Parse a wire `audience` token. Unknown/missing shapes degrade to `party` —
 * the client never invents a third audience and never throws on odd data
 * (older messages may pre-date the field; the server stamps the default).
 */
export function parseAudience(raw?: string | null): Audience {
  if (typeof raw === "string" && raw.startsWith("direct:")) {
    const agentId = raw.slice("direct:".length).trim();
    if (agentId) return { kind: "direct", agentId };
  }
  return { kind: "party" };
}

/**
 * Anything the audience field can carry before it reaches the wire: the parsed
 * in-memory shape, an already-built wire token (round-trip from
 * `buildPostBody`), or empty.
 */
export type AudienceLike = Audience | string | null | undefined;

/**
 * The composer→wire projection: `party` (the server default) is OMITTED so the
 * default post body stays exactly `{ body, parentId? }` — the same
 * minimal-wire discipline buildPostBody has always enforced. Only a direct
 * audience adds the field. Accepts a parsed audience OR an already-built wire
 * token so `buildPostBody` is idempotent (the client re-builds whatever the
 * composer hands it; building twice must be a no-op).
 */
export function audienceWire(audience: AudienceLike): string | undefined {
  const parsed =
    typeof audience === "string" ? parseAudience(audience) : audience ?? null;
  if (!parsed || parsed.kind === "party") return undefined;
  return directAudience(parsed.agentId);
}

/** Short human label for an audience chip / selector option. */
export function audienceLabel(audience: Audience): string {
  return audience.kind === "direct"
    ? `Direct · ${audience.agentId}`
    : "Party (room)";
}
