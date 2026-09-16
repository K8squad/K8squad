// components/overview/HBarChart.tsx — max-normalized horizontal bar chart
// (DESIGN-SPEC-ISI-4505 §5.6). Each row = label · proportional bar · value. Bar widths are
// normalized to the largest value so the biggest bar fills the track. Colors are passed in as
// existing tokens (default: --accent); the chart adds no palette.
//
// Accessibility: the list is role="img" with an aria-label enumerating every label/value pair, so a
// screen reader gets the full ranking. Rows are aria-hidden decoration. Loading / empty states
// degrade honestly (ISI-4229).

import "./overview.css";

export interface HBarItem {
  label: string;
  value: number;
  /** A CSS color for the fill — pass an existing token, e.g. "var(--status-blocked)". */
  color?: string;
}

export function HBarChart({
  items,
  formatValue = (v) => String(v),
  ariaLabel,
  loading = false,
}: {
  items: HBarItem[];
  formatValue?: (v: number) => string;
  ariaLabel?: string;
  loading?: boolean;
}) {
  if (loading) {
    return (
      <div className="ov-hbar" role="status" aria-label="Loading chart">
        {[0, 1, 2].map((i) => (
          <div key={i} className="ov-hbar__row" aria-hidden="true">
            <span className="ov-skel" style={{ height: 12, width: "70%" }} />
            <span className="ov-skel" style={{ height: 10, width: "100%" }} />
            <span className="ov-skel" style={{ height: 12, width: 20 }} />
          </div>
        ))}
      </div>
    );
  }

  const max = Math.max(0, ...items.map((it) => Math.max(0, it.value)));

  if (items.length === 0 || max === 0) {
    return (
      <div className="ov-chart__state" role="img" aria-label={ariaLabel ?? "No data"}>
        <span>No data in this window.</span>
      </div>
    );
  }

  const desc =
    ariaLabel ??
    `Horizontal bar chart: ${items.map((it) => `${it.label} ${formatValue(it.value)}`).join(", ")}`;

  return (
    <div className="ov-hbar" role="img" aria-label={desc}>
      {items.map((it) => (
        <div key={it.label} className="ov-hbar__row" aria-hidden="true">
          <span className="ov-hbar__label" title={it.label}>
            {it.label}
          </span>
          <span className="ov-hbar__track">
            <span
              className="ov-hbar__fill"
              style={{
                width: `${(Math.max(0, it.value) / max) * 100}%`,
                background: it.color,
              }}
            />
          </span>
          <span className="ov-hbar__value">{formatValue(it.value)}</span>
        </div>
      ))}
    </div>
  );
}
