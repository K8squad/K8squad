// test/discussion/threadDigest.test.ts — the pure pre-seed builder for the
// create-tickets-from-thread flow (ISI-7710, UX spec ISI-7709 §3).

import { describe, it, expect } from "vitest";
import { buildThreadDigest } from "@/lib/discussion/threadDigest";
import { KIND_RESET, type Message } from "@/lib/discussion/types";

function msg(id: string, body: string, extra: Partial<Message> = {}): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "author-" + id,
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-10-01T10:00:00Z",
    ...extra,
  };
}

describe("buildThreadDigest", () => {
  it("seeds the title from the thread title and digests the messages", () => {
    const ms = [
      msg("m1", "first message"),
      msg("m2", "second message\nwith a second line"),
      msg("m3", "third message"),
    ];
    const d = buildThreadDigest("Fix the login flake", ms);
    expect(d.title).toBe("Fix the login flake");
    expect(d.body).toContain("- **author-m1**: first message");
    expect(d.body).toContain("- **author-m2**: second message");
    expect(d.body).not.toContain("with a second line");
    expect(d.sourceMessageIds).toEqual(["m1", "m2", "m3"]);
  });

  it("keeps only the last 5 non-marker, non-retracted messages", () => {
    const ms = [
      msg("m0", "too old to cite"),
      msg("m1", "reset marker", { kind: KIND_RESET }),
      msg("m2", "proposal card", { kind: "proposal" }),
      msg("m3", "structured post-back", { kind: "structured" }),
      msg("m4", "retracted", { invalidatedAt: "2026-10-02T10:00:00Z" }),
      msg("n1", "live one"),
      msg("n2", "live two"),
      msg("n3", "live three"),
      msg("n4", "live four"),
      msg("n5", "live five"),
      msg("n6", "live six"),
    ];
    const d = buildThreadDigest("T", ms);
    expect(d.sourceMessageIds).toEqual(["n2", "n3", "n4", "n5", "n6"]);
    expect(d.body).not.toContain("reset marker");
    expect(d.body).not.toContain("proposal card");
    expect(d.body).not.toContain("retracted");
    expect(d.body).toContain("live six");
  });

  it("returns an empty body and ids for an empty or all-marker thread", () => {
    expect(buildThreadDigest("T", [])).toEqual({
      title: "T",
      body: "",
      sourceMessageIds: [],
    });
    const d = buildThreadDigest("T", [msg("m1", "x", { kind: KIND_RESET })]);
    expect(d.body).toBe("");
    expect(d.sourceMessageIds).toEqual([]);
  });

  it("trims the title, caps it at 80 chars, and tolerates absence", () => {
    expect(buildThreadDigest("  padded  ", []).title).toBe("padded");
    expect(buildThreadDigest("x".repeat(100), []).title).toHaveLength(80);
    expect(buildThreadDigest(undefined, []).title).toBe("");
  });
});
