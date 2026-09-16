// components/overview/tone.ts — the semantic tone vocabulary shared by the overview primitives.
//
// A `Tone` is NOT a new color: it names one of the already-shipped status/brand hues
// (globals.css --status-* / --ksq-run / --accent). Primitives translate a tone into the matching
// `ov-tone--*` class (see overview.css) so the whole dashboard speaks the one semantic color
// system — green == running/in-progress everywhere (DESIGN-SPEC-ISI-4505 §2).

export type Tone =
  | "running"
  | "paused"
  | "blocked"
  | "idle"
  | "run"
  | "accent"
  | "neutral";

export function toneClass(tone: Tone | undefined): string {
  return `ov-tone--${tone ?? "neutral"}`;
}

/** The four run states a RunStatusMixBar distributes over, in fixed render order. */
export const MIX_STATES = ["running", "paused", "blocked", "idle"] as const;
export type MixState = (typeof MIX_STATES)[number];
export type RunStatusCounts = Partial<Record<MixState, number>>;
