// Author/provenance badge derivation — the load-bearing crux of Story 10.3
// (AC2 / FR-J3). Every rendered message MUST carry an author/provenance badge
// derived from the 10.1 columns. Attribution is NEVER dropped and NEVER
// fabricated: a message with no derivable author yields a `defect` badge that
// the UI renders as an explicit "unattributed" marker — not a made-up name.
//
// Mapping from the story's provenance triple onto the LANDED 10.1 schema:
//   - agent  — `authorType === "agent"`  (story: `author_agent_id` set)
//   - human  — `authorType === "human"`  (story: `author_agent_id` NULL)
//   - Run    — `metadata.runId` present  (story: `author_run_id` set) → a Run
//              chip that deep-links to the Run detail page (Story 8.11).
// The Run chip is ADDITIVE: an agent posting from within a Run carries both an
// agent badge and a Run chip.

import type { AuthorType, Message } from "./types";

export type BadgeKind = AuthorType | "unknown";

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

const VALID_TYPES: ReadonlySet<string> = new Set<AuthorType>([
  "agent",
  "human",
  "system",
]);

/** The Run-detail route for a given Run id (Story 8.11 deep-link). */
export function runHref(runId: string): string {
  return `/runs/${encodeURIComponent(runId)}`;
}

/**
 * Extract a Run reference from a message's metadata, if any. Accepts either the
 * landed `metadata.runId` convention or the story's `author_run_id` naming so
 * the console is robust to either producer.
 */
export function extractRun(
  metadata: Record<string, unknown> | null | undefined,
): RunRef | undefined {
  if (!metadata) return undefined;
  const raw = metadata["runId"] ?? metadata["author_run_id"];
  if (typeof raw !== "string") return undefined;
  const runId = raw.trim();
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
  m: Pick<Message, "authorType" | "authorName" | "metadata">,
): AuthorBadge {
  const run = extractRun(m.metadata);
  const name = (m.authorName ?? "").trim();
  const kind: BadgeKind = VALID_TYPES.has(m.authorType as string)
    ? (m.authorType as AuthorType)
    : "unknown";

  const label = name || defaultLabel(kind, run);

  // A message is a provenance DEFECT only when nothing at all is derivable:
  // no valid author type, no author name, and no originating Run.
  const defect = kind === "unknown" && name === "" && !run;

  const badge: AuthorBadge = { kind, label, defect };
  if (run) badge.run = run;
  return badge;
}
