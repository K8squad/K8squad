// test/discussion/working.authorReply.test.ts — ISI-7485 (WS-4 of parent
// ISI-7482): applyAuthorReply resolves a working watch from the run's FINAL
// comment echo (a bridged ticket_comment) — the same author-identity
// correlation applyReply uses for an in-thread reply, so the two paths can
// never disagree about which watch an agent resolves.

import { describe, it, expect } from "vitest";
import { applyAuthorReply, seedWatches } from "@/lib/discussion/working";

function seed(names: string[]) {
  return seedWatches([], { messageId: "m1", agentNames: names, now: 1_000 });
}

describe("applyAuthorReply (ISI-7485)", () => {
  it("resolves the matching working watch by run-stamped author", () => {
    let ws = seed(["john", "amy"]);
    ws = applyAuthorReply(ws, "agent:john");
    expect(ws.find((w) => w.agentName === "john")?.phase).toBe("replied");
    expect(ws.find((w) => w.agentName === "amy")?.phase).toBe("working");
  });

  it("matches case-insensitively on the bare name", () => {
    let ws = seed(["John"]);
    ws = applyAuthorReply(ws, "agent:JOHN");
    expect(ws[0].phase).toBe("replied");
  });

  it("resolves the lone working watch when the author matches no name", () => {
    let ws = seed(["john"]);
    ws = applyAuthorReply(ws, "run/abc123");
    expect(ws[0].phase).toBe("replied");
  });

  it("stays hands-off when the author is ambiguous across working watches", () => {
    const ws = seed(["john", "amy"]);
    const out = applyAuthorReply(ws, "run/abc123");
    expect(out.every((w) => w.phase === "working")).toBe(true);
  });

  it("is a no-op with no working watches or an empty author", () => {
    const replied = seed(["john"]).map((w) => ({ ...w, phase: "replied" as const }));
    expect(applyAuthorReply(replied, "agent:john")).toEqual(replied);
    expect(applyAuthorReply(seed(["john"]), "")).toEqual(seed(["john"]));
  });
});
