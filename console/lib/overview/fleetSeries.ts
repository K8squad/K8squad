// lib/overview/fleetSeries.ts — client seam for the fleet overview time-series
// (ISI-5706 WS-1c; GET /api/squad/overview/series), consumed by the ISI-5708
// WS-3 fleet recompose.
//
// The apiserver payload (squadseries.go SquadOverviewSeries) shares the SAME tile
// types as the per-project overview series (projectoverview.go reuses
// TicketsSeriesTile / RunsByStatusTile / TokensTile), so the wire shape is
// structurally identical to the per-project ProjectOverviewSeries — the only
// differences are the endpoint and the `fleet` flag. This module therefore REUSES
// the projectSeries projections verbatim (toAreaSeries, areaXLabels, toRunBars,
// formatTokens) and only owns the fetch + fleet-scoped endpoint, so the fleet and
// per-project charts fold identically and stay in lockstep.

import {
  type ProjectOverviewSeries,
  type SeriesState,
  classifySeriesStatus,
} from "@/lib/overview/projectSeries";

/** The fleet series is the per-project series shape plus the admin `fleet` flag. */
export type FleetOverviewSeries = ProjectOverviewSeries & { fleet?: boolean };

/**
 * Fetch the fleet overview series from the BFF. `window` is the trailing window
 * (default 30d; the apiserver also accepts ?from=&to=). Degrades honestly: a
 * 404/501/network error resolves to a `not-available`/`error` state the panels
 * render as an empty chart — never a fabricated band (degrade-don't-blank).
 */
export async function fetchFleetSeries(
  windowParam = "30d",
  signal?: AbortSignal,
): Promise<SeriesState> {
  const url = `/api/squad/overview/series?window=${encodeURIComponent(windowParam)}`;
  try {
    const res = await fetch(url, { headers: { accept: "application/json" }, signal });
    if (!res.ok) return classifySeriesStatus(res.status);
    const series = (await res.json()) as FleetOverviewSeries;
    return { kind: "ready", series };
  } catch {
    return { kind: "error", status: 0 };
  }
}
