import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchThreadRunEvents,
  hydratedRoomEvents,
  seedLiveThinking,
  type HydratedThreadRun,
} from "@/lib/discussion/hydration";

// ISI-7869 (WS-C of parent ISI-7864): the durable REST hydration shaping —
// the discussion analog of the ISI-7484 runStream hydration tests. Pins the
// contract the room mount relies on: rows fold under the ANCHOR message
// through the live parsers, dedupe by (runId, seq) in both directions, and
// the fetch degrades to null instead of throwing.

function runWith(
  events: Array<{ id: string; type: string; data: unknown }>,
  anchor = "m1",
  runId = "r1",
): HydratedThreadRun {
  return { runId, anchorMessageId: anchor, events };
}

describe("seedLiveThinking", () => {
  it("seeds thinking rows under the anchor message in event order", () => {
    const runs = [
      runWith([
        {
          id: "11",
          type: "thinking",
          data: {
            runId: "r1",
            author: "agent:john",
            body: "step one",
            seq: 1,
            at: "2026-11-26T10:00:00Z",
          },
        },
        {
          id: "12",
          type: "thinking",
          data: {
            runId: "r1",
            author: "agent:john",
            body: "step two",
            seq: 2,
            at: "2026-11-26T10:00:01Z",
          },
        },
      ]),
    ];
    const seeded = seedLiveThinking(runs, {});
    expect(seeded["m1"]).toHaveLength(2);
    expect(seeded["m1"].map((r) => r.body)).toEqual(["step one", "step two"]);
  });

  it("maps ticket_comment rows onto the deliverable tier", () => {
    const runs = [
      runWith([
        {
          id: "13",
          type: "ticket_comment",
          data: {
            runId: "r1",
            workItemId: "w1",
            author: "agent:john",
            body: "final answer",
            at: "2026-11-26T10:05:00Z",
          },
        },
      ]),
    ];
    const seeded = seedLiveThinking(runs, {});
    expect(seeded["m1"]).toHaveLength(1);
    expect(seeded["m1"][0].kind).toBe("deliverable");
    expect(seeded["m1"][0].body).toBe("final answer");
  });

  it("dedupes hydration against live rows already in the map", () => {
    const current = {
      m1: [
        {
          runId: "r1",
          author: "agent:john",
          body: "step one",
          seq: 1,
          at: "2026-11-26T10:00:00Z",
        },
      ],
    };
    const runs = [
      runWith([
        {
          id: "11",
          type: "thinking",
          data: {
            runId: "r1",
            author: "agent:john",
            body: "step one",
            seq: 1,
            at: "2026-11-26T10:00:00Z",
          },
        },
      ]),
    ];
    const seeded = seedLiveThinking(runs, current);
    expect(seeded["m1"]).toHaveLength(1);
  });

  it("keeps live rows never covered by hydration", () => {
    const current = {
      m2: [
        {
          runId: "r2",
          author: "agent:quill",
          body: "live row",
          seq: 3,
          at: "2026-11-26T10:00:02Z",
        },
      ],
    };
    const seeded = seedLiveThinking([], current);
    expect(seeded["m2"]).toHaveLength(1);
  });

  it("drops event types the room never renders", () => {
    const runs = [
      runWith([
        {
          id: "9",
          type: "tool_result",
          data: {
            runId: "r1",
            author: "agent:john",
            body: "x",
            seq: 9,
            at: "2026-11-26T10:00:00Z",
          },
        },
      ]),
    ];
    const seeded = seedLiveThinking(runs, {});
    expect(seeded["m1"]).toBeUndefined();
  });
});

describe("hydratedRoomEvents", () => {
  it("parses discussion_message echoes into room events", () => {
    const message = {
      id: "msg-9",
      threadId: "t1",
      body: "reply",
      createdAt: "2026-11-26T10:06:00Z",
      authorName: "john",
    };
    const runs = [
      runWith([
        {
          id: "14",
          type: "discussion_message",
          data: { type: "message.created", message },
        },
      ]),
    ];
    const evts = hydratedRoomEvents(runs);
    expect(evts).toHaveLength(1);
    expect(evts[0].type).toBe("message.created");
  });

  it("skips non-message rows", () => {
    const runs = [
      runWith([
        {
          id: "11",
          type: "thinking",
          data: {
            runId: "r1",
            author: "a",
            body: "b",
            seq: 1,
            at: "2026-11-26T10:00:00Z",
          },
        },
      ]),
    ];
    expect(hydratedRoomEvents(runs)).toHaveLength(0);
  });
});

describe("fetchThreadRunEvents", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("parses an ok response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({
        ok: true,
        json: async () => ({
          runs: [
            {
              runId: "r1",
              anchorMessageId: "m1",
              events: [{ id: "1", type: "thinking", data: {}, at: "" }],
            },
          ],
        }),
      })) as unknown as typeof fetch,
    );
    const runs = await fetchThreadRunEvents("p", "t");
    expect(runs).toHaveLength(1);
    expect(runs?.[0].runId).toBe("r1");
    expect(runs?.[0].events[0].id).toBe("1");
  });

  it("returns null on a failed fetch (degrade, never throw)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: false, json: async () => ({}) })),
    );
    expect(await fetchThreadRunEvents("p", "t")).toBeNull();
  });

  it("returns null on a malformed body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, json: async () => ({ nope: 1 }) })),
    );
    expect(await fetchThreadRunEvents("p", "t")).toBeNull();
  });
});
