// test/tickets/progressEnvelope.test.ts — ISI-5190 (S1 of ISI-5185).
// The ProgressMirror envelope parser: clean narration, structured tool chips,
// terminal status rows, and a verbatim fallback so nothing is ever dropped.

import { describe, it, expect } from "vitest";
import {
  parseProgressEnvelope,
  envelopeSnippet,
  friendlyToolVerb,
} from "@/lib/tickets/progressEnvelope";

describe("parseProgressEnvelope — narration", () => {
  it("de-prefixes [run …][untrusted] into a clean narration bubble", () => {
    const seg = parseProgressEnvelope(
      "[run cc83e752][untrusted] I'll start by exploring the workspace…",
    );
    expect(seg).toEqual({
      kind: "narration",
      runId: "cc83e752",
      text: "I'll start by exploring the workspace…",
    });
  });

  it("keeps the run id but strips it from the text for a bare [run …] body", () => {
    const seg = parseProgressEnvelope("[run 9952db54] plain narration text");
    expect(seg).toEqual({
      kind: "narration",
      runId: "9952db54",
      text: "plain narration text",
    });
  });

  it("tolerates a bare [untrusted] tag with no run prefix", () => {
    const seg = parseProgressEnvelope("[untrusted] no run id here");
    expect(seg).toEqual({ kind: "narration", runId: undefined, text: "no run id here" });
  });
});

describe("parseProgressEnvelope — tool", () => {
  it("parses the bare [tool:read/result(ok)] row Henrik reported", () => {
    const seg = parseProgressEnvelope("[run cc83e752][tool:read/result(ok)]");
    expect(seg).toMatchObject({
      kind: "tool",
      runId: "cc83e752",
      name: "read",
      phase: "result(ok)",
      verb: "Read file",
      status: "ok",
      done: true,
    });
    expect((seg as { summary?: string }).summary).toBeUndefined();
  });

  it("carries the ok status and the summary when present", () => {
    const seg = parseProgressEnvelope("[run r6][tool:shell/result(ok)] ls -la");
    expect(seg).toMatchObject({
      kind: "tool",
      name: "shell",
      phase: "result(ok)",
      verb: "Ran command",
      status: "ok",
      done: true,
      summary: "ls -la",
    });
  });

  it("lifts err out of result(err)", () => {
    const seg = parseProgressEnvelope("[run r6][tool:edit/result(err)] file not found");
    expect(seg).toMatchObject({
      kind: "tool",
      status: "err",
      done: true,
      verb: "Edited file",
      summary: "file not found",
    });
  });

  it("marks a start phase as in-flight with no status", () => {
    const seg = parseProgressEnvelope("[run r6][tool:shell/start] ls -la");
    expect(seg).toMatchObject({
      kind: "tool",
      phase: "start",
      done: false,
    });
    expect((seg as { status?: string }).status).toBeUndefined();
  });

  it("handles a bare result phase with no outcome", () => {
    const seg = parseProgressEnvelope("[run r6][tool:read/result] decoded from json");
    expect(seg).toMatchObject({ kind: "tool", phase: "result", done: true });
    expect((seg as { status?: string }).status).toBeUndefined();
  });
});

describe("parseProgressEnvelope — status", () => {
  it("parses a terminal status with a reason", () => {
    const seg = parseProgressEnvelope("[run r6][status] failed: exceeded budget");
    expect(seg).toEqual({
      kind: "status",
      runId: "r6",
      state: "failed",
      reason: "exceeded budget",
    });
  });

  it("parses a terminal status without a reason", () => {
    const seg = parseProgressEnvelope("[run r6][status] succeeded");
    expect(seg).toEqual({ kind: "status", runId: "r6", state: "succeeded", reason: undefined });
  });
});

describe("parseProgressEnvelope — fallback", () => {
  it("returns a human comment verbatim as raw", () => {
    const seg = parseProgressEnvelope("Looks good to me, merging.");
    expect(seg).toEqual({ kind: "raw", text: "Looks good to me, merging." });
  });

  it("returns an empty body as empty raw", () => {
    expect(parseProgressEnvelope("")).toEqual({ kind: "raw", text: "" });
  });
});

describe("friendlyToolVerb", () => {
  it("maps known tools case-insensitively", () => {
    expect(friendlyToolVerb("Read")).toBe("Read file");
    expect(friendlyToolVerb("grep")).toBe("Searched code");
    expect(friendlyToolVerb("bash")).toBe("Ran command");
  });

  it("humanizes an unknown tool name", () => {
    expect(friendlyToolVerb("my_custom_tool")).toBe("Used my custom tool");
    expect(friendlyToolVerb("")).toBe("Tool");
  });
});

describe("envelopeSnippet", () => {
  it("de-noises a tool row for the collapsed one-liner", () => {
    const seg = parseProgressEnvelope("[run r6][tool:shell/result(ok)] ls -la");
    expect(envelopeSnippet(seg)).toBe("Ran command (ok): ls -la");
  });

  it("returns narration text unchanged", () => {
    const seg = parseProgressEnvelope("[run r6][untrusted] hello");
    expect(envelopeSnippet(seg)).toBe("hello");
  });
});
