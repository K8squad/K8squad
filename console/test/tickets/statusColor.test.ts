// test/tickets/statusColor.test.ts — the phase-status single-source-of-truth
// (ISI-4452 S1). Guards the hue-token contract shared by List + Kanban and the
// tolerant handling of the current 5-lane read model until ISI-4455 lands.

import { describe, it, expect } from "vitest";
import {
  PHASE_STATUSES,
  statusColor,
  statusMeta,
  STATUS_META,
} from "@/lib/tickets/statusColor";

describe("statusColor / statusMeta (S1)", () => {
  it("exposes all 10 phase statuses in lifecycle order (design §1)", () => {
    expect(PHASE_STATUSES).toEqual([
      "backlog",
      "todo",
      "design",
      "planning",
      "implementation",
      "code_review",
      "testing",
      "documentation",
      "done",
      "cancelled",
    ]);
  });

  it("maps each phase to its own --ksq-phase-* token", () => {
    for (const s of PHASE_STATUSES) {
      expect(statusColor(s)).toBe(`var(--ksq-phase-${s})`);
    }
  });

  it("carries group + owning role for the mid-phase Build/Review statuses", () => {
    expect(statusMeta("planning")).toMatchObject({ group: "Build", role: "Architect" });
    expect(statusMeta("code_review")).toMatchObject({ group: "Review", role: "Code Reviewer" });
    expect(statusMeta("testing")).toMatchObject({ group: "Review", role: "Testing Architect" });
    expect(statusMeta("documentation")).toMatchObject({ group: "Review", role: "Tech Writer" });
  });

  it("marks cancelled as struck-through", () => {
    expect(statusMeta("cancelled").struck).toBe(true);
  });

  it("resolves the legacy 5-lane states to the closest phase hue (pre-ISI-4455)", () => {
    // in_progress / in_review still ride the current read-model enum.
    expect(statusColor("in_progress")).toBe("var(--ksq-phase-implementation)");
    expect(statusColor("in_review")).toBe("var(--ksq-phase-code_review)");
    expect(statusMeta("in_progress").label).toBe("In Progress");
  });

  it("degrades an unknown status to a neutral chip, never a fabricated colour", () => {
    const meta = statusMeta("nonexistent_phase");
    expect(meta.hue).toBe("var(--ksq-phase-backlog)");
    expect(meta.label).toBe("Nonexistent Phase");
    expect(STATUS_META["nonexistent_phase"]).toBeUndefined();
  });
});
