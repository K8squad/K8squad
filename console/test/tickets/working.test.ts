// test/tickets/working.test.ts — ISI-5196 (S5 of ISI-5185, Track A).
// The ticket-view port of the ISI-5174 "an agent is working…" affordance: the
// honest-ladder → WorkingPhase projection, the live step-verb label enrichment,
// and the composed pill view.

import { describe, it, expect } from "vitest";
import {
  phaseFromDispatchState,
  stepVerbFromThinking,
  ticketWorkingLabel,
  ticketWorkingView,
} from "@/lib/tickets/working";

describe("phaseFromDispatchState — ladder → WorkingPhase", () => {
  it("maps the live ladder states to `working`", () => {
    expect(phaseFromDispatchState("queued")).toBe("working");
    expect(phaseFromDispatchState("picking_up")).toBe("working");
    expect(phaseFromDispatchState("working")).toBe("working");
  });
  it("maps succeeded → replied and failed → failed", () => {
    expect(phaseFromDispatchState("succeeded")).toBe("replied");
    expect(phaseFromDispatchState("failed")).toBe("failed");
  });
});

describe("stepVerbFromThinking — current step from live thinking", () => {
  it("returns null for no rows", () => {
    expect(stepVerbFromThinking(undefined)).toBeNull();
    expect(stepVerbFromThinking([])).toBeNull();
  });

  it("projects a tool segment into a present-progressive verb + target", () => {
    const verb = stepVerbFromThinking([
      { summary: "[run cc83e752][tool:edit/start] MessageItem.tsx" },
    ]);
    expect(verb).toBe("editing MessageItem.tsx");
  });

  it("uses only the LATEST row (the current step)", () => {
    const verb = stepVerbFromThinking([
      { summary: "[run cc83e752][tool:read/result(ok)] old.ts" },
      { summary: "[run cc83e752][tool:bash/start]" },
    ]);
    expect(verb).toBe("running a command");
  });

  it("gives no verb for narration — the caller falls back to 'is working…'", () => {
    expect(
      stepVerbFromThinking([
        { summary: "[run cc83e752][untrusted] Looking at the failing test…" },
      ]),
    ).toBeNull();
  });

  it("gives no verb for a status row or an empty body", () => {
    expect(
      stepVerbFromThinking([{ summary: "[run cc83e752][status] succeeded" }]),
    ).toBeNull();
    expect(stepVerbFromThinking([{ summary: "" }])).toBeNull();
  });

  it("humanizes an unknown tool name rather than leaking a literal", () => {
    const verb = stepVerbFromThinking([
      { summary: "[run cc83e752][tool:custom_probe/start]" },
    ]);
    expect(verb).toBe("using custom probe");
  });

  it("truncates a long tool target to one line", () => {
    const long = "a/very/deep/path/that/keeps/going/and/going/and/going/file.ts";
    const verb = stepVerbFromThinking([
      { summary: `[run cc83e752][tool:read/start] ${long}` },
    ]);
    expect(verb).not.toBeNull();
    expect((verb as string).length).toBeLessThanOrEqual("reading ".length + 48);
    expect(verb).toMatch(/…$/);
  });
});

describe("ticketWorkingLabel — copy per phase", () => {
  it("working with a step verb reads '{agent} is {verb}…'", () => {
    expect(ticketWorkingLabel("Fragua", "working", "editing MessageItem.tsx")).toBe(
      "Fragua is editing MessageItem.tsx…",
    );
  });
  it("working without a step verb falls back to '{agent} is working…'", () => {
    expect(ticketWorkingLabel("Fragua", "working", null)).toBe(
      "Fragua is working…",
    );
  });
  it("mirrors the discussion copy for replied / failed", () => {
    expect(ticketWorkingLabel("Fragua", "replied", null)).toBe("Fragua replied");
    expect(ticketWorkingLabel("Fragua", "failed", null)).toBe(
      "Fragua could not respond",
    );
  });
});

describe("ticketWorkingView — composed pill", () => {
  it("returns null with no agent to attribute the work to", () => {
    expect(ticketWorkingView("", "working", [])).toBeNull();
  });

  it("enriches the working label with the live step verb", () => {
    const view = ticketWorkingView("Fragua", "working", [
      { summary: "[run cc83e752][tool:edit/start] MessageItem.tsx" },
    ]);
    expect(view).toEqual({
      agentName: "Fragua",
      phase: "working",
      label: "Fragua is editing MessageItem.tsx…",
    });
  });

  it("ignores the step verb once terminal (phase tells the whole truth)", () => {
    expect(
      ticketWorkingView("Fragua", "succeeded", [
        { summary: "[run cc83e752][tool:edit/start] MessageItem.tsx" },
      ]),
    ).toEqual({ agentName: "Fragua", phase: "replied", label: "Fragua replied" });

    expect(ticketWorkingView("Fragua", "failed", undefined)).toEqual({
      agentName: "Fragua",
      phase: "failed",
      label: "Fragua could not respond",
    });
  });

  it("falls back to the plain working label while queued with no thinking yet", () => {
    expect(ticketWorkingView("Fragua", "queued", undefined)).toEqual({
      agentName: "Fragua",
      phase: "working",
      label: "Fragua is working…",
    });
  });
});
