// components/overview/StatTile.tsx — a single KPI tile for the 5-up overview stat band
// (DESIGN-SPEC-ISI-4505 §5.1). Label · big semantic-colored value · sub-line · corner glyph.
// Pure presentational; `loading` swaps the value/sub for skeletons (degrade-don't-blank, ISI-4229).

import type { ReactNode } from "react";
import { toneClass, type Tone } from "./tone";
import "./overview.css";

export function StatTile({
  label,
  value,
  tone,
  sub,
  glyph,
  loading = false,
}: {
  label: string;
  value: ReactNode;
  /** Semantic hue for the value (reuses an existing status/brand token). Default: neutral text-hi. */
  tone?: Tone;
  sub?: ReactNode;
  /** Optional corner glyph (icon). Rendered decorative (aria-hidden). */
  glyph?: ReactNode;
  loading?: boolean;
}) {
  return (
    <div
      className={`ov-stat${loading ? " ov-stat--loading" : ""}`}
      role="group"
      aria-label={label}
    >
      <span className="ov-stat__label">{label}</span>
      {loading ? (
        <span className="ov-skel ov-skel--value" aria-hidden="true" />
      ) : (
        <span className={`ov-stat__value ${toneClass(tone)}`}>{value}</span>
      )}
      {loading ? (
        <span className="ov-skel ov-skel--sub" aria-hidden="true" />
      ) : sub != null ? (
        <span className="ov-stat__sub">{sub}</span>
      ) : null}
      {glyph != null && (
        <span className="ov-stat__glyph" aria-hidden="true">
          {glyph}
        </span>
      )}
    </div>
  );
}

/** Responsive 5-up band wrapper. Children are StatTiles. */
export function StatBand({ children }: { children: ReactNode }) {
  return <div className="ov-stat-band">{children}</div>;
}
