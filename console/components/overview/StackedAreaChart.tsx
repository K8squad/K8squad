// components/overview/StackedAreaChart.tsx — time × status stacked-area chart
// (DESIGN-SPEC-ISI-4505 §5.5). Renders one filled band per status series, stacked, over a shared
// time axis. Colors are passed in as existing tokens (e.g. "var(--status-running)") — the chart
// introduces no palette of its own.
//
// Accessibility: the <svg> is role="img" with an aria-label summarising the series and window, a
// <title>, plus a visible legend and a source caption. Bands are aria-hidden decoration; the label
// carries the meaning. Loading / empty states degrade honestly (ISI-4229) — never a blank frame.

import type { ReactNode } from "react";
import "./overview.css";

export interface AreaSeries {
  key: string;
  label: string;
  /** A CSS color — pass an existing token, e.g. "var(--status-running)". */
  color: string;
  /** One value per x-slot; all series must be the same length. */
  values: number[];
}

const VIEW_W = 600;
const VIEW_H = 200;
const PAD = { top: 8, right: 8, bottom: 8, left: 8 };

export function StackedAreaChart({
  series,
  xLabels,
  caption,
  ariaLabel,
  loading = false,
}: {
  series: AreaSeries[];
  /** Optional x-axis labels (used only for the accessible description). */
  xLabels?: string[];
  caption?: ReactNode;
  ariaLabel?: string;
  loading?: boolean;
}) {
  const n = series[0]?.values.length ?? 0;

  if (loading) {
    return (
      <div className="ov-chart">
        <div className="ov-chart__state" role="status" aria-label="Loading chart">
          <span className="ov-skel" style={{ width: "100%", height: 120 }} />
        </div>
      </div>
    );
  }

  // Column totals drive the y-scale; if everything is zero (or <2 points) there's nothing to plot.
  const totals = Array.from({ length: n }, (_, x) =>
    series.reduce((sum, s) => sum + Math.max(0, s.values[x] ?? 0), 0),
  );
  const maxTotal = Math.max(0, ...totals);

  if (n < 2 || maxTotal === 0) {
    return (
      <div className="ov-chart">
        <div className="ov-chart__state" role="img" aria-label={ariaLabel ?? "No time-series data"}>
          <span>Not enough history yet.</span>
        </div>
        {caption && <span className="ov-chart__caption">{caption}</span>}
      </div>
    );
  }

  const innerW = VIEW_W - PAD.left - PAD.right;
  const innerH = VIEW_H - PAD.top - PAD.bottom;
  const xAt = (i: number) => PAD.left + (i / (n - 1)) * innerW;
  const yAt = (v: number) => PAD.top + innerH - (v / maxTotal) * innerH;

  // Stack: each series sits on the cumulative baseline of the series below it.
  const baselines = Array.from({ length: n }, () => 0);
  const bands = series.map((s) => {
    const top: string[] = [];
    const bottom: string[] = [];
    for (let i = 0; i < n; i++) {
      const val = Math.max(0, s.values[i] ?? 0);
      const y0 = baselines[i];
      const y1 = y0 + val;
      bottom.push(`${xAt(i).toFixed(1)},${yAt(y0).toFixed(1)}`);
      top.push(`${xAt(i).toFixed(1)},${yAt(y1).toFixed(1)}`);
      baselines[i] = y1;
    }
    // top edge left→right, then bottom edge right→left, closed.
    const points = [...top, ...bottom.reverse()].join(" ");
    return { key: s.key, color: s.color, points };
  });

  const desc =
    ariaLabel ??
    `Stacked area chart of ${series.map((s) => s.label).join(", ")} over ${n}` +
      ` ${xLabels ? "points" : "time points"}` +
      (xLabels && xLabels.length ? ` from ${xLabels[0]} to ${xLabels[xLabels.length - 1]}` : "");

  return (
    <div className="ov-chart">
      <svg
        className="ov-chart__svg"
        viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={desc}
      >
        <title>{desc}</title>
        {bands.map((b) => (
          <polygon key={b.key} points={b.points} fill={b.color} fillOpacity={0.85} aria-hidden="true" />
        ))}
      </svg>
      <div className="ov-chart__legend" aria-hidden="true">
        {series.map((s) => (
          <span key={s.key} className="ov-chart__legend-item">
            <span className="ov-chart__swatch" style={{ color: s.color }} />
            {s.label}
          </span>
        ))}
      </div>
      {caption && <span className="ov-chart__caption">{caption}</span>}
    </div>
  );
}
