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
  /** An authoritative count, or `null` for an UNAVAILABLE bucket (no backing data) — the row
   * then renders an em-dash with no bar rather than a fabricated 0 (FR-I3, ISI-5738). */
  value: number | null;
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

  // The scale ignores unavailable (null) buckets — they contribute no bar, only an em-dash.
  const max = Math.max(
    0,
    ...items.map((it) => (it.value == null ? 0 : Math.max(0, it.value))),
  );

  if (items.length === 0 || max === 0) {
    return (
      <div
        className="ov-chart__state"
        role="img"
        aria-label={ariaLabel ?? "No data"}
      >
        <span>No data in this window.</span>
      </div>
    );
  }

  const desc =
    ariaLabel ??
    `Horizontal bar chart: ${items
      .map(
        (it) =>
          `${it.label} ${it.value == null ? "unavailable" : formatValue(it.value)}`,
      )
      .join(", ")}`;

  return (
    <div className="ov-hbar" role="img" aria-label={desc}>
      {items.map((it) => {
        const unavailable = it.value == null;
        return (
          <div
            key={it.label}
            className="ov-hbar__row"
            data-unavailable={unavailable || undefined}
            aria-hidden="true"
          >
            <span className="ov-hbar__label" title={it.label}>
              {it.label}
            </span>
            <span className="ov-hbar__track">
              {/* Unavailable bucket: no fill at all (not even the 2px min-width sliver) — FR-I3. */}
              {!unavailable && (
                <span
                  className="ov-hbar__fill"
                  style={{
                    width: `${(Math.max(0, it.value as number) / max) * 100}%`,
                    background: it.color,
                  }}
                />
              )}
            </span>
            <span
              className="ov-hbar__value"
              title={
                unavailable ? "Not available on this deployment" : undefined
              }
            >
              {unavailable ? "—" : formatValue(it.value as number)}
            </span>
          </div>
        );
      })}
    </div>
  );
}
