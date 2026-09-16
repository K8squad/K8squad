import { describe, it, expect } from "vitest";
import {
  areaXLabels,
  classifySeriesStatus,
  formatTokens,
  toAreaSeries,
  toRunBars,
  type TicketsSnapshot,
} from "@/lib/overview/projectSeries";

// ISI-4508 (S3) — the pure client projections over the ISI-4509 S4 read model. Coverage focuses on
// the contract seams: the mock's 4 area bands (todo folds into Backlog), the client-side RunPhase
// bucketing (5 bars, blocked≡failed), token compaction, and the degrade-vs-error classification.

describe("toAreaSeries", () => {
  const snaps: TicketsSnapshot[] = [
    { date: "2026-09-14", counts: { backlog: 3, todo: 1, in_progress: 2, in_review: 1, done: 8 } },
    { date: "2026-09-15", counts: { backlog: 2, todo: 0, in_progress: 3, in_review: 1, done: 8 } },
  ];

  it("folds the §13 enum into the mock's four bands (todo → Backlog)", () => {
    const series = toAreaSeries(snaps);
    expect(series.map((s) => s.label)).toEqual(["Backlog", "In progress", "In review", "Done"]);
    // Backlog band = backlog + todo per snapshot.
    expect(series[0].values).toEqual([4, 2]);
    expect(series[1].values).toEqual([2, 3]); // in_progress
    expect(series[3].values).toEqual([8, 8]); // done
  });

  it("uses only existing status/brand tokens for band colors (no new palette)", () => {
    for (const s of toAreaSeries(snaps)) expect(s.color).toMatch(/^var\(--/);
  });

  it("zero-fills a missing status key rather than NaN", () => {
    const [backlog] = toAreaSeries([{ date: "2026-09-16", counts: {} }]);
    expect(backlog.values).toEqual([0]);
  });
});

describe("areaXLabels", () => {
  it("shortens ISO dates to MM-DD for the accessible axis description", () => {
    expect(areaXLabels([{ date: "2026-09-14", counts: {} }])).toEqual(["09-14"]);
  });
});

describe("toRunBars", () => {
  it("buckets raw RunPhase counts into the mock's five bars", () => {
    const bars = toRunBars({ running: 2, succeeded: 12, paused: 1, cancelled: 3, failed: 1 });
    const by = Object.fromEntries(bars.map((b) => [b.label, b.value]));
    expect(by).toEqual({ Completed: 12, Running: 2, Paused: 1, Cancelled: 3, Failed: 1 });
  });

  it("is case-insensitive and merges phase spellings into one bucket", () => {
    const bars = toRunBars({ RUNNING: 1, claiming: 1, Canceled: 2, cancelled: 1 });
    const by = Object.fromEntries(bars.map((b) => [b.label, b.value]));
    expect(by.Running).toBe(2); // RUNNING + claiming
    expect(by.Cancelled).toBe(3); // Canceled + cancelled
  });

  it("renders every bar (zeroed) even when a phase is absent", () => {
    expect(toRunBars({}).map((b) => b.value)).toEqual([0, 0, 0, 0, 0]);
  });
});

describe("formatTokens", () => {
  it("compacts millions/thousands and passes small counts through", () => {
    expect(formatTokens(1_243_566)).toBe("1.2M");
    expect(formatTokens(812_345)).toBe("812.3k");
    expect(formatTokens(947)).toBe("947");
  });

  it("degrades to an em dash when the sum is unavailable", () => {
    expect(formatTokens(undefined)).toBe("—");
  });
});

describe("classifySeriesStatus", () => {
  it("treats 404/501 as the honest degrade seam, not an error", () => {
    expect(classifySeriesStatus(404).kind).toBe("not-available");
    expect(classifySeriesStatus(501).kind).toBe("not-available");
  });

  it("treats other statuses as retryable errors", () => {
    expect(classifySeriesStatus(500).kind).toBe("error");
    expect(classifySeriesStatus(0).kind).toBe("error");
  });
});
