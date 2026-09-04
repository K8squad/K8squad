// lib/modelHints.ts — the curated model-hints seam (Story B, ISI-3555 / feature ISI-3544).
//
// The single source the Agent model selector reads its curated Claude ids from, behind one hook
// `useModelHints()`. For the MVP this is a STATIC list baked into the console (ISI-3546 / Winston
// decision, 2026-09-02): there is intentionally NO `GET /api/models/hints` endpoint — that is
// Story C / Phase 2. The seam exists so Phase 2 can swap the source (static → fetch) with zero
// change to the component tree or its ACs (technical note "one seam, two sources").
//
// Vendor neutrality (ADR-026): this curated list is a CONVENIENCE, never a CONSTRAINT. The
// selector must always keep "Custom model…" (any id) and "Bring your own endpoint" reachable, and
// no otherwise-valid `model` string may be excluded by construction (AC7). Nothing here filters or
// gates what the user may ultimately submit — it only offers common choices one click away.

/** One curated model offered up front: the CRD `model` id verbatim + a human label. */
export interface ModelHint {
  /** The exact `model` string written to the Agent CRD when this hint is chosen (AC1). */
  id: string;
  /** Human-facing label for the option (AC1 "each with a human label"). */
  label: string;
}

/**
 * The static curated Claude family (Opus / Sonnet / Haiku). MVP source of `useModelHints()`.
 *
 * These are convenience defaults only; a user needing any other id reaches it through
 * "Custom model…" (AC2/AC7). Ordered most-capable → fastest so the common first pick is on top.
 * Frozen so a consumer cannot mutate the shared list.
 */
export const CURATED_MODELS: readonly ModelHint[] = Object.freeze([
  { id: "claude-opus-4-8", label: "Claude Opus 4.8 — most capable" },
  { id: "claude-sonnet-5", label: "Claude Sonnet 5 — balanced" },
  { id: "claude-opus-5", label: "Claude Opus 5" },
  { id: "claude-sonnet-4-5", label: "Claude Sonnet 4.5" },
  { id: "claude-haiku-4-5", label: "Claude Haiku 4.5 — fastest" },
]);

/**
 * The model-hints hook. MVP returns the static curated list synchronously; Story C swaps the body
 * to fetch `GET /api/models/hints` behind the SAME return shape so no consumer changes (task 1,
 * the single swap point). It is a plain function (no React state) today — the `use` prefix marks
 * the seam and lets Phase 2 introduce loading/error state without a call-site rename.
 */
export function useModelHints(): readonly ModelHint[] {
  return CURATED_MODELS;
}

/** True when `id` is one of the curated hints (drives edit-mode "curated vs Custom" — AC1/AC2). */
export function isCuratedModel(id: string, hints: readonly ModelHint[] = CURATED_MODELS): boolean {
  const v = id.trim();
  return v.length > 0 && hints.some((h) => h.id === v);
}

/**
 * Soft, non-blocking guidance for a Custom model id whose shape is unusual (AC5: "surfaces
 * guidance (soft — do not hard-block a legal-but-unusual id)"). Returns a hint string when the id
 * does not look like a conventional model id / DNS-ish token, else undefined. NEVER an error: a
 * legal-but-unusual id must still submit (vendor neutrality, AC7). Empty is not this function's
 * concern — required-ness is enforced by `validate()` in lib/compose.
 */
export function modelShapeHint(id: string): string | undefined {
  const v = id.trim();
  if (!v) return undefined; // emptiness is a hard "required" error elsewhere, not a soft hint
  // Conventional model ids are lowercase alphanumerics with '-', '.', ':' or '/' separators
  // (e.g. claude-opus-4-8, ollama/llama3.1:8b, gpt-4o). Anything with whitespace or uppercase is
  // probably a typo — nudge, don't block.
  if (!/^[a-z0-9]([a-z0-9._:/-]*[a-z0-9])?$/.test(v)) {
    return "Unusual model id — double-check it (letters, digits and - . : / only). It will still be submitted as-is.";
  }
  return undefined;
}
