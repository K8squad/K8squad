// test/tickets/transitions.test.ts — the client-side phase-transition guard
// (ISI-4457 S4). Pure logic, no DOM: proves the fold + the fail-closed adjacency
// the Kanban renders as no-drop cues.

import { describe, it, expect } from "vitest";
import {
  ALLOWED_DROPS,
  allowedTargets,
  canTransition,
  phaseOf,
} from "@/lib/tickets/transitions";
import { PHASE_STATUSES } from "@/lib/tickets/statusColor";

describe("phaseOf — read-model state → phase column", () => {
  it("maps every canonical phase status to itself", () => {
    for (const s of PHASE_STATUSES) expect(phaseOf(s)).toBe(s);
  });

  it("folds the transitional engine lanes onto their column (never a column of their own)", () => {
    expect(phaseOf("in_progress")).toBe("implementation");
    expect(phaseOf("in_review")).toBe("code_review");
  });

  it("returns null for an unknown state (dropped, never guessed)", () => {
    expect(phaseOf("wat")).toBeNull();
    expect(phaseOf("")).toBeNull();
  });
});

describe("canTransition — fail-closed drop guard", () => {
  it("allows a forward move along the lifecycle", () => {
    expect(canTransition("implementation", "code_review")).toBe(true);
    expect(canTransition("code_review", "testing")).toBe(true);
    expect(canTransition("testing", "done")).toBe(true);
  });

  it("allows the sanctioned rework hops", () => {
    expect(canTransition("code_review", "implementation")).toBe(true);
    expect(canTransition("testing", "code_review")).toBe(true);
  });

  it("blocks a disallowed long jump", () => {
    expect(canTransition("backlog", "done")).toBe(false);
    expect(canTransition("planning", "documentation")).toBe(false);
  });

  it("blocks the direct terminal↔terminal hop (the one the server 422s)", () => {
    expect(canTransition("done", "cancelled")).toBe(false);
    expect(canTransition("cancelled", "done")).toBe(false);
  });

  it("allows reopening a terminal only onto the intake lanes", () => {
    expect(canTransition("done", "todo")).toBe(true);
    expect(canTransition("cancelled", "backlog")).toBe(true);
    expect(canTransition("done", "implementation")).toBe(false);
  });

  it("treats a same-column drop as a no-op, not a move", () => {
    expect(canTransition("implementation", "implementation")).toBe(false);
    // legacy lane folds to its column, so an in_progress→implementation drop is a no-op too
    expect(canTransition("in_progress", "implementation")).toBe(false);
  });

  it("honours the fold on the origin side (legacy lane uses its column's adjacency)", () => {
    expect(canTransition("in_progress", "code_review")).toBe(true); // == implementation→code_review
    expect(canTransition("in_review", "documentation")).toBe(true); // == code_review→documentation
  });

  it("never moves an unknown origin (fail-closed)", () => {
    expect(canTransition("wat", "todo")).toBe(false);
  });
});

describe("allowedTargets — drives the guard-aware quick-move menu", () => {
  it("returns the origin phase's adjacency and never includes the origin itself", () => {
    const targets = allowedTargets("implementation");
    expect(targets).toEqual(ALLOWED_DROPS.implementation);
    expect(targets).not.toContain("implementation");
  });

  it("returns nothing for an unknown origin", () => {
    expect(allowedTargets("nope")).toEqual([]);
  });
});
