// lib/overview/projectSeries.ts — the client seam for the project-overview read model
// (ISI-4508 S3, backed by the ISI-4509 S4 endpoint GET /api/projects/{id}/overview).
//
// One endpoint, three series (S4-READ-API-CONTRACT-ISI-4509): a daily tickets-by-status
// snapshot list (stacked area), raw RunPhase counts (h-bars, bucketed CLIENT-side per the
// contract), and a window token sum (stat tile). Everything degrades HONESTLY: a 404/501/网
// network error, or an `available: false` seam, resolves to a `not-available` shape the panels render
// as an empty state — never a fake band (degrade-don't-blank, carried from ISI-4229).
//
// The endpoint ships in PR #495 and is not live on every deployment yet, so the fetch MUST NOT
// throw the page down when it 404s — the dashboard's non-S4 panels (issues, runs, agents) keep
// working from the reads the console already issues.

import { AreaSeries } from "@/components/overview/StackedAreaChart";
import { HBarItem } from "@/components/overview/HBarChart";

// ---------------------------------------------------------------------------
// Wire shapes (S4 contract §Response — ProjectOverviewSeries).
// ---------------------------------------------------------------------------

export interface TicketsSnapshot {
  date: string;
  counts: Partial<Record<"backlog" | "todo" | "in_progress" | "in_review" | "done", number>>;
}

export interface ProjectOverviewSeries {
  project?: { name: string; namespace: string };
  window?: { from: string; to: string; bucket: string };
  ticketsByStatus?: { available: boolean; snapshots?: TicketsSnapshot[] | null; reason?: string };
  runsByStatus?: { available: boolean; byPhase?: Record<string, number> | null; total?: number };
  tokens?: {
    available: boolean;
    input?: number;
    output?: number;
    total?: number;
    runsCounted?: number;
    estimatedUsd?: number | null;
  };
}

// The read-model resolves to one of these; `not-available` is the honest degrade (endpoint
// unrolled on this deployment, or the caller can't see the project) — NOT an error to surface.
export type SeriesState =
  | { kind: "loading" }
  | { kind: "not-available"; status: number }
  | { kind: "error"; status: number }
  | { kind: "ready"; series: ProjectOverviewSeries };

// ---------------------------------------------------------------------------
// Pure projections (exported for unit tests — no fetch, no DOM).
// ---------------------------------------------------------------------------

// The four stacked-area bands the approved mock shows (§3). The §13 enum has five states; `todo`
// folds into Backlog so the chart matches the mock's Backlog / In progress / In review / Done.
export const AREA_BANDS = [
  { key: "backlog", label: "Backlog", color: "var(--status-idle)", from: ["backlog", "todo"] },
  { key: "in_progress", label: "In progress", color: "var(--status-running)", from: ["in_progress"] },
  { key: "in_review", label: "In review", color: "var(--accent)", from: ["in_review"] },
  { key: "done", label: "Done", color: "var(--ksq-run)", from: ["done"] },
] as const;

/** Fold the daily per-status snapshots into the mock's four stacked-area bands. */
export function toAreaSeries(snapshots: TicketsSnapshot[]): AreaSeries[] {
  return AREA_BANDS.map((band) => ({
    key: band.key,
    label: band.label,
    color: band.color,
    values: snapshots.map((snap) =>
      band.from.reduce((sum, s) => sum + Math.max(0, snap.counts[s] ?? 0), 0),
    ),
  }));
}

/** Short axis labels (MM-DD) for the accessible description of the area chart. */
export function areaXLabels(snapshots: TicketsSnapshot[]): string[] {
  return snapshots.map((s) => s.date.slice(5)); // "2026-09-14" → "09-14"
}

// Raw RunPhase → display bucket (S4 contract §Notes). The read model stays honest with raw
// phases; the client owns the display bucketing. `blocked` is a work-item condition, not a run
// phase, so the mock's "Blocked" slot maps to the failed bucket.
const RUN_BUCKETS = [
  { label: "Completed", color: "var(--status-running)", phases: ["succeeded"] },
  { label: "Running", color: "var(--accent)", phases: ["pending", "claiming", "dispatching", "running", "collecting"] },
  { label: "Paused", color: "var(--status-paused)", phases: ["paused"] },
  { label: "Cancelled", color: "var(--status-idle)", phases: ["canceling", "cancelling", "cancelled", "canceled"] },
  { label: "Failed", color: "var(--status-blocked)", phases: ["failed"] },
] as const;

/** Bucket the raw RunPhase counts into the mock's five h-bars (lossless input, display fold). */
export function toRunBars(byPhase: Record<string, number>): HBarItem[] {
  const norm = new Map<string, number>();
  for (const [phase, n] of Object.entries(byPhase ?? {})) {
    norm.set(phase.toLowerCase(), (norm.get(phase.toLowerCase()) ?? 0) + Math.max(0, n));
  }
  return RUN_BUCKETS.map((b) => ({
    label: b.label,
    color: b.color,
    value: b.phases.reduce((sum, p) => sum + (norm.get(p) ?? 0), 0),
  }));
}

/** Compact token count for the stat tile ("1.2M", "812.3k", "947"). */
export function formatTokens(total: number | undefined): string {
  if (total == null || !Number.isFinite(total)) return "—";
  if (total >= 1_000_000) return `${(total / 1_000_000).toFixed(1)}M`;
  if (total >= 1_000) return `${(total / 1_000).toFixed(1)}k`;
  return String(total);
}

// ---------------------------------------------------------------------------
// Fetch (thin — the BFF proxies the §13 choke point; we only classify degrade vs error).
// ---------------------------------------------------------------------------

/**
 * Classify a non-ok status from the overview endpoint. 404 (project not visible / endpoint not
 * mounted) and 501 (read model unwired) are the DEGRADE path — the charts show honest empty
 * states, the rest of the dashboard is unaffected. Everything else is a retryable error.
 */
export function classifySeriesStatus(status: number): Exclude<SeriesState, { kind: "ready" | "loading" }> {
  return status === 404 || status === 501
    ? { kind: "not-available", status }
    : { kind: "error", status };
}

/** Fetch the project-overview series from the BFF. `window` is the trailing window (default 30d). */
export async function fetchProjectSeries(
  projectId: string,
  windowParam = "30d",
  signal?: AbortSignal,
): Promise<SeriesState> {
  const url = `/api/projects/${encodeURIComponent(projectId)}/overview?window=${encodeURIComponent(windowParam)}`;
  try {
    const res = await fetch(url, { headers: { accept: "application/json" }, signal });
    if (!res.ok) return classifySeriesStatus(res.status);
    const series = (await res.json()) as ProjectOverviewSeries;
    return { kind: "ready", series };
  } catch {
    // AbortError or a network failure — treat as a retryable error (not a degrade seam).
    return { kind: "error", status: 0 };
  }
}
