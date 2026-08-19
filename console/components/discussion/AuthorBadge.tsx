// AuthorBadge — renders the author/provenance badge for a message (AC2/FR-J3).
// It NEVER fabricates an author: a defect badge renders an explicit
// "unattributed" chip. A Run-originated message additionally renders a Run chip
// that deep-links to the Run detail page (Story 8.11).
//
// Theming (AC6 / 8.9): the chip border derives from its BASE colour, not a
// hardcoded value — the theme-invariant `#{BASE}55` rule. Because the base is
// an 8.9 theme CSS variable (not a literal hex) here, the JS side only carries
// the base as a `--chip-base` custom property; the stylesheet
// (`discussion.css`) finalizes the border with
// `color-mix(in srgb, var(--chip-base) 33%, transparent)` — the CSS analogue of
// the `#{BASE}55` interpolation. `chipBorder()` models the same transform for
// hex bases (see lib/discussion/theme.ts / test/discussion/theme.test.ts).

import type { CSSProperties } from "react";
import type { AuthorBadge as Badge } from "@/lib/discussion/provenance";
import { badgeBaseColor, runChipBaseColor } from "@/lib/discussion/theme";

function chipStyle(base: string): CSSProperties {
  // Custom properties are not part of the CSSProperties type; cast narrowly.
  return { ["--chip-base"]: base } as CSSProperties;
}

export function AuthorBadge({ badge }: { badge: Badge }) {
  const label = badge.defect ? "unattributed" : badge.label;
  return (
    <span className="ksq-author" data-testid="author-badge">
      <span
        className="ksq-chip"
        data-testid="author-chip"
        data-kind={badge.kind}
        data-defect={badge.defect ? "true" : "false"}
        style={chipStyle(badgeBaseColor(badge.kind))}
      >
        {label}
      </span>
      {badge.run ? (
        <a
          className="ksq-chip ksq-chip--run"
          data-testid="run-chip"
          data-run-id={badge.run.runId}
          href={badge.run.href}
          style={chipStyle(runChipBaseColor())}
        >
          Run
        </a>
      ) : null}
    </span>
  );
}
