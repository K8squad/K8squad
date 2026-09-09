// Author/provenance badge derivation — the load-bearing crux of Story 10.3
// (AC2 / FR-J3). Every rendered message MUST carry an author/provenance badge
// derived from the 10.1 columns. Attribution is NEVER dropped and NEVER
// fabricated: a message with no derivable author yields a `defect` badge that
// the UI renders as an explicit "unattributed" marker — not a made-up name.
//
// Mapping from the story's provenance triple onto the REAL 10.1 schema
// (ISI-4016 — the columns the apiserver actually emits, per store.go):
//   - agent  — `authorAgentId` present   (backend `AuthorKind()` ⇒ "agent")
//   - human  — `authorAgentId` absent    (backend `AuthorKind()` ⇒ "human")
//   - Run    — `authorRunId` present      → a Run chip that deep-links to the
//              Run detail page (Story 8.11).
// The Run chip is ADDITIVE: an agent posting from within a Run carries both an
// agent badge and a Run chip. The label comes from `authorPrincipal`.

import type { Message } from "./types";

// `system` is retained as a theme token (see theme.ts) but is NOT derivable
// from the real schema — the backend only distinguishes agent vs human.
export type BadgeKind = "agent" | "human" | "system" | "unknown";

/** A deep-link reference to the originating Run (Story 8.11 Run detail). */
export interface RunRef {
  runId: string;
  href: string;
}

export interface AuthorBadge {
  kind: BadgeKind;
  /** Human-visible author label (never fabricated when `defect` is true). */
  label: string;
  /** Present iff the message originated from a Run. */
  run?: RunRef;
  /**
   * True when no author/provenance could be derived from the message. The UI
   * must render this as a visible "unattributed" marker — a message with no
   * visible author is a defect (AC2), so we surface it rather than hide it.
   */
  defect: boolean;
}

/** The Run-detail route for a given Run id (Story 8.11 deep-link). */
export function runHref(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`;
}

/**
 * Extract a Run reference from a message's `authorRunId`, if any. The value is
 * the server-stamped Run origin column (`internal/discussion/store.go`); a
 * blank or missing id yields no chip.
 */
export function extractRun(
  authorRunId: string | null | undefined,
): RunRef | undefined {
  if (typeof authorRunId !== "string") return undefined;
  const runId = authorRunId.trim();
  if (runId === "") return undefined;
  return { runId, href: runHref(runId) };
}

function defaultLabel(kind: BadgeKind, run: RunRef | undefined): string {
  switch (kind) {
    case "agent":
      return "Agent";
    case "human":
      return "Human";
    case "system":
      return "System";
    default:
      return run ? "Run" : "";
  }
}

/**
 * Derive the author/provenance badge for a message. Exhaustive over the
 * provenance triple; never throws; never fabricates an author.
 */
export function deriveAuthorBadge(
  m: Pick<Message, "authorPrincipal" | "authorAgentId" | "authorRunId">,
): AuthorBadge {
  const run = extractRun(m.authorRunId);
  const name = (m.authorPrincipal ?? "").trim();
  const agentId = (m.authorAgentId ?? "").trim();

  // Backend `AuthorKind()`: an author is an agent iff author_agent_id is set,
  // otherwise a human. Only when NOTHING identifies the author (no agent id and
  // no principal) do we fall back to "unknown" rather than fabricate a kind.
  const kind: BadgeKind =
    agentId !== "" ? "agent" : name !== "" ? "human" : "unknown";

  const label = name || defaultLabel(kind, run);

  // A message is a provenance DEFECT only when nothing at all is derivable:
  // no agent id, no principal, and no originating Run.
  const defect = agentId === "" && name === "" && !run;

  const badge: AuthorBadge = { kind, label, defect };
  if (run) badge.run = run;
  return badge;
}
