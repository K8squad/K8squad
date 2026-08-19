import { describe, it, expect } from "vitest";
import { nestMessages, countThread } from "@/lib/discussion/thread";
import type { Message } from "@/lib/discussion/types";

function msg(id: string, parentId: string | null, createdAt: string): Message {
  return {
    id,
    roomId: "room",
    parentId,
    authorId: "a",
    authorType: "human",
    authorName: "u",
    body: id,
    kind: "message",
    createdAt,
  };
}

// AC1: threaded history — adjacency (parentId) → tree, stable order.

describe("nestMessages — AC1 threading", () => {
  it("nests replies under parents and orders roots by createdAt", () => {
    const flat = [
      msg("r2", null, "2026-08-17T10:02:00Z"),
      msg("r1", null, "2026-08-17T10:00:00Z"),
      msg("c1", "r1", "2026-08-17T10:01:00Z"),
      msg("c2", "r1", "2026-08-17T10:03:00Z"),
    ];
    const roots = nestMessages(flat);
    expect(roots.map((m) => m.id)).toEqual(["r1", "r2"]);
    expect(roots[0].replies!.map((m) => m.id)).toEqual(["c1", "c2"]);
    expect(roots[1].replies).toEqual([]);
  });

  it("nests recursively (reply to a reply)", () => {
    const flat = [
      msg("r1", null, "t0"),
      msg("c1", "r1", "t1"),
      msg("g1", "c1", "t2"),
    ];
    const roots = nestMessages(flat);
    expect(countThread(roots[0])).toBe(3);
    expect(roots[0].replies![0].replies![0].id).toBe("g1");
  });

  it("treats an orphan (parent not present) as a root — never dropped", () => {
    const flat = [msg("c1", "missing-parent", "t0")];
    const roots = nestMessages(flat);
    expect(roots.map((m) => m.id)).toEqual(["c1"]);
  });

  it("does not mutate input", () => {
    const flat = [msg("r1", null, "t0"), msg("c1", "r1", "t1")];
    nestMessages(flat);
    expect(flat[0].replies).toBeUndefined();
  });
});
