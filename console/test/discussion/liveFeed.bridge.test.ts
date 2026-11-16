// test/discussion/liveFeed.bridge.test.ts — ISI-7485 (WS-4 of parent
// ISI-7482): the bridged project-bus row decoders — parseTicketCommentRow /
// parseRunEndedRow / ticketCommentToDeliverable — defensive exactly like
// parseThinkingRow: garbage never throws, an unrenderable frame is null.

import { describe, it, expect } from "vitest";
import {
  parseTicketCommentRow,
  parseRunEndedRow,
  ticketCommentToDeliverable,
} from "@/lib/discussion/liveFeed";

const echo = {
  workItemId: "wi-1",
  runId: "run-9",
  author: "agent:john",
  body: "final recap: shipped the bridge",
  kind: "summary",
  at: "2026-11-16T09:00:00Z",
};

describe("parseTicketCommentRow — the ISI-7274 echo on the project bus", () => {
  it("decodes a LiveCommentEcho frame", () => {
    expect(parseTicketCommentRow(JSON.stringify(echo))).toEqual({
      runId: "run-9",
      workItemId: "wi-1",
      author: "agent:john",
      body: "final recap: shipped the bridge",
      kind: "summary",
      at: "2026-11-16T09:00:00Z",
    });
  });

  it("tolerates a plain comment (no kind) and missing ids", () => {
    const row = parseTicketCommentRow(
      JSON.stringify({ author: "agent:amy", body: "done", at: "" }),
    );
    expect(row).toEqual({
      runId: "",
      workItemId: "",
      author: "agent:amy",
      body: "done",
      at: "",
    });
  });

  it("rejects a frame with no body, no author, or garbage", () => {
    expect(parseTicketCommentRow(JSON.stringify({ author: "a" }))).toBeNull();
    expect(parseTicketCommentRow(JSON.stringify({ body: "b" }))).toBeNull();
    expect(parseTicketCommentRow("not json")).toBeNull();
    expect(parseTicketCommentRow(JSON.stringify(null))).toBeNull();
  });
});

describe("parseRunEndedRow — the bridged terminal milestone", () => {
  const frame = {
    schema: "ksquad.lifecycle.v1",
    event: "run.ended",
    from_step: "collecting",
    to_step: "succeeded",
    fence_token: 7,
    run_id: "run-9",
    work_item_id: "wi-1",
    agent: "john",
  };

  it("decodes a lifecycle v1 run.ended frame to the surface-minimal row", () => {
    expect(parseRunEndedRow(JSON.stringify(frame))).toEqual({
      runId: "run-9",
      agent: "john",
    });
  });

  it("decodes a frame with no agent stamp", () => {
    expect(
      parseRunEndedRow(JSON.stringify({ ...frame, agent: undefined })),
    ).toEqual({ runId: "run-9" });
  });

  it("rejects a frame that names no run or another domain event", () => {
    expect(parseRunEndedRow(JSON.stringify({ event: "run.ended" }))).toBeNull();
    expect(
      parseRunEndedRow(
        JSON.stringify({ event: "work_item.assigned", run_id: "wi-1" }),
      ),
    ).toBeNull();
    expect(parseRunEndedRow("not json")).toBeNull();
  });
});

describe("ticketCommentToDeliverable — the deliverable-tier mapping", () => {
  it("maps the echo onto the run's ONE-final-answer tier with a stable seq", () => {
    const row = ticketCommentToDeliverable({
      runId: "run-9",
      workItemId: "wi-1",
      author: "agent:john",
      body: "final recap",
      kind: "summary",
      at: "2026-11-16T09:00:00Z",
    });
    expect(row).toEqual({
      runId: "run-9",
      author: "agent:john",
      body: "final recap",
      seq: Date.parse("2026-11-16T09:00:00Z"),
      at: "2026-11-16T09:00:00Z",
      kind: "deliverable",
    });
  });

  it("falls back to seq 0 when at is missing/unparseable (still dedup-safe)", () => {
    const row = ticketCommentToDeliverable({
      runId: "run-9",
      workItemId: "wi-1",
      author: "agent:john",
      body: "final recap",
      at: "",
    });
    expect(row.seq).toBe(0);
    expect(row.kind).toBe("deliverable");
  });
});
