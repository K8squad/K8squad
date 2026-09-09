import { describe, it, expect } from "vitest";
import {
  deriveAuthorBadge,
  extractRun,
  runHref,
} from "@/lib/discussion/provenance";

// AC2 (the crux): badge derivation is exhaustive over the REAL provenance triple
// (authorPrincipal / authorAgentId / authorRunId — ISI-4016).

describe("deriveAuthorBadge — AC2 provenance triple", () => {
  it("agent: authorAgentId present → agent badge with the principal label", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "planner-1",
      authorAgentId: "agent-abc",
      authorRunId: null,
    });
    expect(b.kind).toBe("agent");
    expect(b.label).toBe("planner-1");
    expect(b.defect).toBe(false);
    expect(b.run).toBeUndefined();
  });

  it("human: authorAgentId absent → human badge", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "henrik",
      authorAgentId: null,
      authorRunId: null,
    });
    expect(b.kind).toBe("human");
    expect(b.label).toBe("henrik");
    expect(b.defect).toBe(false);
  });

  it("agent with no principal falls back to the 'Agent' label (never blank)", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "",
      authorAgentId: "agent-xyz",
      authorRunId: null,
    });
    expect(b.kind).toBe("agent");
    expect(b.label).toBe("Agent");
    expect(b.defect).toBe(false);
  });

  it("Run: authorRunId present → Run chip deep-linking to 8.11 (ADDITIVE)", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "coder-2",
      authorAgentId: "agent-2",
      authorRunId: "11111111-2222-3333-4444-555555555555",
    });
    expect(b.kind).toBe("agent"); // Run chip is ADDITIVE to the author badge
    expect(b.run).toBeDefined();
    expect(b.run!.runId).toBe("11111111-2222-3333-4444-555555555555");
    expect(b.run!.href).toBe("/runs/11111111-2222-3333-4444-555555555555");
  });

  it("DEFECT: no agent id, no principal, no run → defect, never a fabricated author", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "",
      authorAgentId: null,
      authorRunId: null,
    });
    expect(b.kind).toBe("unknown");
    expect(b.label).toBe(""); // no fabricated name
    expect(b.defect).toBe(true);
  });

  it("NOT a defect when a principal survives even without an agent id", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "legacy-user",
      authorAgentId: null,
      authorRunId: null,
    });
    expect(b.defect).toBe(false);
    expect(b.kind).toBe("human");
    expect(b.label).toBe("legacy-user");
  });

  it("NOT a defect when only a Run is derivable", () => {
    const b = deriveAuthorBadge({
      authorPrincipal: "",
      authorAgentId: null,
      authorRunId: "r1",
    });
    expect(b.defect).toBe(false);
    expect(b.label).toBe("Run");
    expect(b.run!.href).toBe("/runs/r1");
  });
});

describe("extractRun / runHref", () => {
  it("ignores blank / non-string / missing run ids", () => {
    expect(extractRun("   ")).toBeUndefined();
    expect(extractRun(42 as unknown as string)).toBeUndefined();
    expect(extractRun(null)).toBeUndefined();
    expect(extractRun(undefined)).toBeUndefined();
  });
  it("url-encodes the run id in the deep link", () => {
    expect(runHref("a b/c")).toBe("/runs/a%20b%2Fc");
  });
});
