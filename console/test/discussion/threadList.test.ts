// Unit coverage for the Discussions-panel pure helpers (ISI-5673 / ISI-5598 WS-C):
// recency ordering, `?thread` → active-thread resolution, optimistic upsert, and
// the relative "last-activity" label (backed by createdAt — the only timestamp
// the thread-list read exposes).

import { describe, it, expect } from "vitest";
import {
  relativeActivity,
  resolveActiveThreadId,
  sortThreadsByRecency,
  upsertThread,
} from "@/lib/discussion/threadList";
import type { Thread } from "@/lib/discussion/types";

function thread(over: Partial<Thread> & { id: string; createdAt: string }): Thread {
  return {
    projectId: "p1",
    teamId: "t1",
    title: `Thread ${over.id}`,
    createdBy: "alice",
    ...over,
  };
}

const NOW = Date.parse("2026-10-12T12:00:00Z");

describe("relativeActivity", () => {
  it("formats seconds/minutes/hours/days ago", () => {
    expect(relativeActivity("2026-10-12T11:59:30Z", NOW)).toBe("30s ago");
    expect(relativeActivity("2026-10-12T11:30:00Z", NOW)).toBe("30m ago");
    expect(relativeActivity("2026-10-12T09:00:00Z", NOW)).toBe("3h ago");
    expect(relativeActivity("2026-10-09T12:00:00Z", NOW)).toBe("3d ago");
  });
  it("clamps a future timestamp to 'just now' and ignores a bad date", () => {
    expect(relativeActivity("2026-10-12T12:01:00Z", NOW)).toBe("just now");
    expect(relativeActivity("not-a-date", NOW)).toBe("");
  });
});

describe("sortThreadsByRecency", () => {
  it("orders newest createdAt first and is stable for ties", () => {
    const a = thread({ id: "a", createdAt: "2026-10-10T00:00:00Z" });
    const b = thread({ id: "b", createdAt: "2026-10-12T00:00:00Z" });
    const c = thread({ id: "c", createdAt: "2026-10-12T00:00:00Z" }); // tie with b
    const out = sortThreadsByRecency([a, b, c]);
    expect(out.map((t) => t.id)).toEqual(["b", "c", "a"]);
  });
  it("does not mutate the input", () => {
    const input = [
      thread({ id: "a", createdAt: "2026-10-10T00:00:00Z" }),
      thread({ id: "b", createdAt: "2026-10-12T00:00:00Z" }),
    ];
    const snapshot = input.map((t) => t.id);
    sortThreadsByRecency(input);
    expect(input.map((t) => t.id)).toEqual(snapshot);
  });
});

describe("resolveActiveThreadId", () => {
  const list = [
    thread({ id: "newest", createdAt: "2026-10-12T00:00:00Z" }),
    thread({ id: "older", createdAt: "2026-10-10T00:00:00Z" }),
  ];
  it("honours a valid ?thread request", () => {
    expect(resolveActiveThreadId(list, "older")).toBe("older");
  });
  it("falls back to the first (newest) thread for a missing/stale request", () => {
    expect(resolveActiveThreadId(list, null)).toBe("newest");
    expect(resolveActiveThreadId(list, "ghost")).toBe("newest");
  });
  it("returns null when the list is empty", () => {
    expect(resolveActiveThreadId([], "whatever")).toBeNull();
  });
});

describe("upsertThread", () => {
  it("inserts a new thread at the top (newest-first)", () => {
    const existing = [thread({ id: "a", createdAt: "2026-10-10T00:00:00Z" })];
    const fresh = thread({ id: "b", createdAt: "2026-10-12T00:00:00Z" });
    expect(upsertThread(existing, fresh).map((t) => t.id)).toEqual(["b", "a"]);
  });
  it("replaces an existing thread by id (no duplicate)", () => {
    const existing = [
      thread({ id: "a", createdAt: "2026-10-12T00:00:00Z", title: "old" }),
    ];
    const updated = thread({
      id: "a",
      createdAt: "2026-10-12T00:00:00Z",
      title: "new",
    });
    const out = upsertThread(existing, updated);
    expect(out).toHaveLength(1);
    expect(out[0].title).toBe("new");
  });
});
