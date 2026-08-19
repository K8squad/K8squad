import { describe, it, expect } from "vitest";
import {
  deriveAuthorBadge,
  extractRun,
  runHref,
} from "@/lib/discussion/provenance";

// AC2 (the crux): badge derivation is exhaustive over the provenance triple.

const base = {
  authorName: "",
  metadata: null as Record<string, unknown> | null,
};

describe("deriveAuthorBadge — AC2 provenance triple", () => {
  it("agent: authorType=agent → agent badge with the agent name", () => {
    const b = deriveAuthorBadge({
      authorType: "agent",
      authorName: "planner-1",
      metadata: null,
    });
    expect(b.kind).toBe("agent");
    expect(b.label).toBe("planner-1");
    expect(b.defect).toBe(false);
    expect(b.run).toBeUndefined();
  });

  it("human: authorType=human → human badge", () => {
    const b = deriveAuthorBadge({
      authorType: "human",
      authorName: "henrik",
      metadata: null,
    });
    expect(b.kind).toBe("human");
    expect(b.label).toBe("henrik");
    expect(b.defect).toBe(false);
  });

  it("system: authorType=system → system badge", () => {
    const b = deriveAuthorBadge({
      authorType: "system",
      authorName: "",
      metadata: null,
    });
    expect(b.kind).toBe("system");
    expect(b.label).toBe("System");
    expect(b.defect).toBe(false);
  });

  it("Run: metadata.runId present → Run chip deep-linking to 8.11", () => {
    const b = deriveAuthorBadge({
      authorType: "agent",
      authorName: "coder-2",
      metadata: { runId: "11111111-2222-3333-4444-555555555555" },
    });
    expect(b.kind).toBe("agent"); // Run chip is ADDITIVE to the author badge
    expect(b.run).toBeDefined();
    expect(b.run!.runId).toBe("11111111-2222-3333-4444-555555555555");
    expect(b.run!.href).toBe("/runs/11111111-2222-3333-4444-555555555555");
  });

  it("Run: accepts the story's author_run_id metadata naming too", () => {
    const b = deriveAuthorBadge({
      ...base,
      authorType: "system",
      metadata: { author_run_id: "run-abc" },
    });
    expect(b.run!.runId).toBe("run-abc");
  });

  it("DEFECT: no type, no name, no run → defect, never a fabricated author", () => {
    const b = deriveAuthorBadge({
      authorType: "" as unknown as "agent",
      authorName: "",
      metadata: null,
    });
    expect(b.kind).toBe("unknown");
    expect(b.label).toBe(""); // no fabricated name
    expect(b.defect).toBe(true);
  });

  it("NOT a defect when a name survives even if type is unknown", () => {
    const b = deriveAuthorBadge({
      authorType: "bogus" as unknown as "agent",
      authorName: "legacy-user",
      metadata: null,
    });
    expect(b.defect).toBe(false);
    expect(b.label).toBe("legacy-user");
  });

  it("NOT a defect when only a Run is derivable", () => {
    const b = deriveAuthorBadge({
      authorType: "" as unknown as "agent",
      authorName: "",
      metadata: { runId: "r1" },
    });
    expect(b.defect).toBe(false);
    expect(b.label).toBe("Run");
    expect(b.run!.href).toBe("/runs/r1");
  });
});

describe("extractRun / runHref", () => {
  it("ignores blank / non-string run ids", () => {
    expect(extractRun({ runId: "   " })).toBeUndefined();
    expect(extractRun({ runId: 42 as unknown as string })).toBeUndefined();
    expect(extractRun(null)).toBeUndefined();
    expect(extractRun(undefined)).toBeUndefined();
  });
  it("url-encodes the run id in the deep link", () => {
    expect(runHref("a b/c")).toBe("/runs/a%20b%2Fc");
  });
});
