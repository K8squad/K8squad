import { describe, it, expect } from "vitest";
import { deriveAgentStatus, statusLabel } from "@/lib/agents/status";

// Story 8.10: the org diagram paints a four-value derived status from the Run phase (§8) +
// Paused sub-reason. These lock the single derivation so the SSE-live repaint and the
// server-rendered snapshot never disagree.

describe("deriveAgentStatus — Run phase → org-diagram bucket", () => {
  it("Running → running", () => {
    expect(deriveAgentStatus("Running")).toBe("running");
  });
  it("Paused → paused", () => {
    expect(deriveAgentStatus("Paused")).toBe("paused");
  });
  it("Pending / Claiming → blocked (admitted, not progressing)", () => {
    expect(deriveAgentStatus("Pending")).toBe("blocked");
    expect(deriveAgentStatus("Claiming")).toBe("blocked");
  });
  it("terminal phases → idle (agent is free)", () => {
    expect(deriveAgentStatus("Succeeded")).toBe("idle");
    expect(deriveAgentStatus("Failed")).toBe("idle");
    expect(deriveAgentStatus("Cancelled")).toBe("idle");
  });
  it("no current Run (null/undefined) → idle", () => {
    expect(deriveAgentStatus(null)).toBe("idle");
    expect(deriveAgentStatus(undefined)).toBe("idle");
  });
  it("an explicit blocked condition overrides the phase", () => {
    expect(deriveAgentStatus("Running", { blockedCondition: true })).toBe(
      "blocked",
    );
  });
});

describe("statusLabel — paused sub-reason (story 7.6)", () => {
  it("surfaces the rate-limit sub-reason", () => {
    expect(statusLabel("paused", "rate_limited")).toBe("paused: rate-limited");
  });
  it("surfaces the credential sub-reason", () => {
    expect(statusLabel("paused", "credential")).toBe("paused: credential");
  });
  it("plain status when there is no sub-reason", () => {
    expect(statusLabel("running")).toBe("running");
    expect(statusLabel("paused")).toBe("paused");
  });
});
