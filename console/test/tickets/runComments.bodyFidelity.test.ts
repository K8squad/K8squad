// test/tickets/runComments.bodyFidelity.test.ts — ISI-7197 (WS-5): the
// issue-thread comment path is a byte-for-byte passthrough — the shaping
// layer (buildRunComments) never escapes, rewrites, or entity-encodes the
// producer's body, so any artifact visible in a rendered bubble is present
// in the stored source text (producer side), not introduced by the FE.

import { describe, it, expect } from "vitest";
import { buildRunComments } from "@/lib/tickets/runComments";
import type { NormalizedThread } from "@/lib/tickets/thread";

const FENCE = "\u0060\u0060\u0060";

const SAMPLE = [
  "## What changed",
  "",
  FENCE + "go",
  "func main() {}",
  FENCE,
  "",
  "Trailing entity.&#x20;",
  "Double &amp;#x20; stays literal.",
].join("\n");

function thread(over: Partial<NormalizedThread> = {}): NormalizedThread {
  return {
    workItemId: "wi-1",
    title: "t",
    description: "",
    state: "done",
    blockedReason: "",
    comments: [],
    changeRefs: [],
    statusHistory: [],
    holder: "",
    runId: "",
    ...over,
  } as NormalizedThread;
}

describe("buildRunComments — body fidelity (ISI-7197)", () => {
  it("passes the agent comment body through byte-for-byte", () => {
    const t = thread({
      comments: [
        {
          author: "agent:Builder",
          body: SAMPLE,
          createdAt: "2026-11-07T10:00:00Z",
        },
      ],
    });
    const bubbles = buildRunComments(t);
    expect(bubbles.length).toBe(1);
    expect(bubbles[0].body).toBe(SAMPLE);
    expect(bubbles[0].authorKind).toBe("agent");
  });
});