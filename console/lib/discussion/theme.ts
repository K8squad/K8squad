// Theming tokens for the room (Story 10.3 AC6 / Story 8.9 whole-shell T1–T7).
// The 8.9 lesson (ISI-2279): a channel/badge chip's BORDER derives from its
// BASE colour, not a hardcoded value — `#{BASE}55` — so it is THEME-INVARIANT
// (identical in dark and light). We keep that invariant as a pure function so
// the theming snapshot contract is testable.

import type { BadgeKind } from "./provenance";

/** Theme-invariant alpha suffix applied to a base colour for chip borders. */
export const CHIP_BORDER_ALPHA = "55";

/**
 * Derive a chip border colour from its base. Pure function of the base only —
 * NOT of the active theme — so a badge's border is identical in dark and light
 * (the 8.9 theme-invariant chip rule). `base` is an 8.9 token or #RRGGBB hex.
 */
export function chipBorder(base: string): string {
  return `${base}${CHIP_BORDER_ALPHA}`;
}

/**
 * Base colour token per badge kind. These reference 8.9 theme CSS variables so
 * the room inherits the whole-shell dark+light palette rather than hardcoding
 * colours. The Run chip and the "unattributed" defect chip get their own
 * tokens so they are visually distinct without ever fabricating an author.
 */
export function badgeBaseColor(kind: BadgeKind): string {
  switch (kind) {
    case "agent":
      return "var(--ksq-badge-agent)";
    case "human":
      return "var(--ksq-badge-human)";
    case "system":
      return "var(--ksq-badge-system)";
    default:
      return "var(--ksq-badge-unknown)";
  }
}

/** Base colour token for the Run deep-link chip. */
export function runChipBaseColor(): string {
  return "var(--ksq-badge-run)";
}
