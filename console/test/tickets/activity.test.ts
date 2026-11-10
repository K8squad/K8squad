// test/tickets/activity.test.ts — the two-phase comment UI's per-run Activity
// feed shaping (ISI-7095 / ISI-7088). groupActivityByRun is the pure layer
// behind the "Activity (N events)" expander: it buckets the kind='progress' feed
// per run, orders the runs chronologically, and trails a legacy (run-id-less)
// bucket last. fetchRunActivity is the thin BFF read normalized into ThreadComment.

import { describe, it, expect, vi, afterEach } from "vitest";
import {
  groupActivityByRun,
  activityEventCount,
  runLabel,
  fetchRunActivity,
} from "@/lib/tickets/activity";
import type { ThreadComment } from "@/lib/tickets/thread";

const progress = (
  runId: string,
  createdAt: string,
  body = "step",
): ThreadComment => ({
  author: `run/${runId.split("-")[0]}`,
  body,
  createdAt,
  kind: "progress",
  runId,
});

describe("runLabel", () => {
  it("takes the run id's leading 8-char token", () => {
    expect(runLabel("abcd1234-0000-0000-0000-000000000000")).toBe("abcd1234");
  });
  it("labels a run-id-less (legacy) bucket 'earlier'", () => {
    expect(runLabel("")).toBe("earlier");
    expect(runLabel("   ")).toBe("earlier");
  });
});

describe("groupActivityByRun", () => {
  it("buckets progress rows per run, events oldest-first within a run", () => {
    const groups = groupActivityByRun([
      progress("r2", "2026-11-05T10:05:00Z", "b"),
      progress("r2", "2026-11-05T10:04:00Z", "a"),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].runId).toBe("r2");
    expect(groups[0].events.map((e) => e.body)).toEqual(["a", "b"]);
    expect(groups[0].at).toBe("2026-11-05T10:04:00Z");
  });

  it("orders the runs chronologically by their first event", () => {
    const groups = groupActivityByRun([
      progress("late", "2026-11-05T12:00:00Z"),
      progress("early", "2026-11-05T09:00:00Z"),
      progress("late", "2026-11-05T12:01:00Z"),
      progress("early", "2026-11-05T09:01:00Z"),
    ]);
    expect(groups.map((g) => g.runId)).toEqual(["early", "late"]);
  });

  it("collapses run-id-less rows into ONE 'earlier' bucket, trailing last", () => {
    const groups = groupActivityByRun([
      progress("", "2026-11-05T08:00:00Z", "legacy-a"),
      progress("r1", "2026-11-05T09:00:00Z", "fresh"),
      progress("", "2026-11-05T08:30:00Z", "legacy-b"),
    ]);
    expect(groups.map((g) => g.runId)).toEqual(["r1", ""]);
    const legacy = groups.find((g) => g.runId === "")!;
    expect(legacy.label).toBe("earlier");
    expect(legacy.events).toHaveLength(2);
  });

  it("is input-order independent (sorts regardless of arrival order)", () => {
    const rows = [
      progress("r1", "2026-11-05T09:02:00Z", "c"),
      progress("r1", "2026-11-05T09:00:00Z", "a"),
      progress("r1", "2026-11-05T09:01:00Z", "b"),
    ];
    expect(groupActivityByRun(rows)[0].events.map((e) => e.body)).toEqual([
      "a",
      "b",
      "c",
    ]);
  });

  it("empty feed yields no groups and a zero count", () => {
    const groups = groupActivityByRun([]);
    expect(groups).toEqual([]);
    expect(activityEventCount(groups)).toBe(0);
  });
});

describe("activityEventCount", () => {
  it("sums events across every run bucket", () => {
    const groups = groupActivityByRun([
      progress("r1", "2026-11-05T09:00:00Z"),
      progress("r1", "2026-11-05T09:01:00Z"),
      progress("r2", "2026-11-05T10:00:00Z"),
    ]);
    expect(activityEventCount(groups)).toBe(3);
  });
});

describe("fetchRunActivity", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("normalizes the lowercase wire array into ThreadComment with kind + runId", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify([
              {
                author: "run/abcd1234",
                body: "[tool:read/result(ok)] done",
                createdAt: "2026-11-05T10:00:00Z",
                kind: "progress",
                runId: "abcd1234-0000-0000-0000-000000000000",
              },
            ]),
            { status: 200 },
          ),
      ),
    );
    const got = await fetchRunActivity("wi-1");
    expect(got).toHaveLength(1);
    expect(got[0].kind).toBe("progress");
    expect(got[0].runId).toBe("abcd1234-0000-0000-0000-000000000000");
  });

  it("throws ApiError on a non-2xx (so the caller degrades to no expander)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("not wired", { status: 501 })),
    );
    await expect(fetchRunActivity("wi-1")).rejects.toMatchObject({
      status: 501,
    });
  });

  it("tolerates a non-array body as empty", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("null", { status: 200 })),
    );
    expect(await fetchRunActivity("wi-1")).toEqual([]);
  });
});
