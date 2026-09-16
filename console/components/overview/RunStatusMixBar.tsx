// components/overview/RunStatusMixBar.tsx — a segmented distribution bar built from a
// {running,paused,blocked,idle} count map (DESIGN-SPEC-ISI-4505 §5.3). Each segment is width-
// proportional to its share of the total; hues reuse the semantic --status-* tokens.
//
// Accessible: the bar is a single role=img with an aria-label summarising the mix ("3 running,
// 1 paused, 0 blocked, 2 idle"), so a screen reader gets the distribution without per-segment noise.
// Empty (all zero) degrades to a flat inset track labelled "no runs" — never a blank/broken bar.

import { MIX_STATES, type RunStatusCounts } from "./tone";
import "./overview.css";

export function RunStatusMixBar({
  counts,
  className,
}: {
  counts: RunStatusCounts;
  className?: string;
}) {
  const values = MIX_STATES.map((s) => Math.max(0, counts[s] ?? 0));
  const total = values.reduce((a, b) => a + b, 0);

  const label =
    total === 0
      ? "no runs"
      : MIX_STATES.map((s, i) => `${values[i]} ${s}`).join(", ");

  if (total === 0) {
    return (
      <div
        className={`ov-mixbar ov-mixbar--empty${className ? ` ${className}` : ""}`}
        role="img"
        aria-label={label}
      />
    );
  }

  return (
    <div
      className={`ov-mixbar${className ? ` ${className}` : ""}`}
      role="img"
      aria-label={label}
    >
      {MIX_STATES.map((s, i) =>
        values[i] > 0 ? (
          <span
            key={s}
            className={`ov-mixbar__seg ov-mixbar__seg--${s}`}
            style={{ width: `${(values[i] / total) * 100}%` }}
          />
        ) : null,
      )}
    </div>
  );
}
