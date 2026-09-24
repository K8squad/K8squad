// test/tickets/RailAssigneeChip.test.tsx — ISI-4882 (S4 of ISI-4853): the rail assignee chip
// tri-state. Covers the AC: the chip label/tone tracks the honest ladder across all four ladder
// states, and falls back cleanly to today's static chip when no dispatch is in flight — with zero
// new tokens (the tint is a verbatim `--status-*` hue on the shared `.ksq-chip--phase`).
//
// The presentational `AssigneeChipView` takes pure props (agent + the S1 `DispatchWatch`), so every
// state is driven directly without mounting the hook / faking fetch + EventSource.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import {
  AssigneeChipView,
  railAssigneeChip,
} from "@/components/tickets/RailAssigneeChip";
import {
  toDispatchWatch,
  type DispatchState,
  type DispatchWatch,
} from "@/lib/tickets/useDispatchWatch";

// Build the public DispatchWatch for a given ladder state via the same derivation the hook uses,
// so label/tone here are exactly what production emits (no hand-forged view drift).
function watchAt(state: DispatchState): DispatchWatch {
  return toDispatchWatch({ state, degrade: "none" });
}

const STATIC = <span data-testid="static-fallback">Requested: agent-x · dispatch pending</span>;

describe("railAssigneeChip — pure derivation (localRunBadge grammar)", () => {
  it("labels each ladder state as '<agent> <verb>'", () => {
    expect(railAssigneeChip("claude", watchAt("queued"))?.label).toBe("claude queued");
    expect(railAssigneeChip("claude", watchAt("picking_up"))?.label).toBe("claude picking up…");
    expect(railAssigneeChip("claude", watchAt("working"))?.label).toBe("claude working…");
    expect(railAssigneeChip("claude", watchAt("succeeded"))?.label).toBe("claude finished");
    expect(railAssigneeChip("claude", watchAt("failed"))?.label).toBe("claude failed");
  });

  it("maps each state to an EXISTING status hue token (zero new tokens)", () => {
    expect(railAssigneeChip("a", watchAt("queued"))?.hue).toBe("var(--status-idle)");
    expect(railAssigneeChip("a", watchAt("picking_up"))?.hue).toBe("var(--status-paused)");
    expect(railAssigneeChip("a", watchAt("working"))?.hue).toBe("var(--status-running)");
    expect(railAssigneeChip("a", watchAt("succeeded"))?.hue).toBe("var(--status-running)");
    expect(railAssigneeChip("a", watchAt("failed"))?.hue).toBe("var(--status-blocked)");
  });

  it("is null (⇒ static fallback) when there is no watch or no agent", () => {
    expect(railAssigneeChip("claude", null)).toBeNull();
    expect(railAssigneeChip("", watchAt("working"))).toBeNull();
    expect(railAssigneeChip("   ", watchAt("working"))).toBeNull();
  });
});

describe("AssigneeChipView — render across the four chip states", () => {
  const cases: Array<[DispatchState, string]> = [
    ["queued", "agent-x queued"],
    ["picking_up", "agent-x picking up…"],
    ["working", "agent-x working…"],
    ["succeeded", "agent-x finished"],
  ];

  for (const [state, label] of cases) {
    it(`renders the ${state} chip with its status label + tone, not the fallback`, () => {
      render(
        <AssigneeChipView agent="agent-x" watch={watchAt(state)} fallback={STATIC} />,
      );
      const chip = screen.getByTestId("detail-assignee-chip");
      expect(chip).toHaveTextContent(label);
      expect(chip).toHaveAttribute("data-state", state);
      expect(chip).toHaveAttribute("role", "status");
      // Tone rides the existing `--phase-hue` custom property — no new class/token.
      expect(chip.getAttribute("style") ?? "").toContain("--phase-hue");
      expect(screen.queryByTestId("static-fallback")).toBeNull();
    });
  }
});

describe("AssigneeChipView — idle fallback", () => {
  it("renders today's static chip verbatim when no dispatch is in flight (null watch)", () => {
    render(<AssigneeChipView agent="agent-x" watch={null} fallback={STATIC} />);
    expect(screen.getByTestId("static-fallback")).toBeInTheDocument();
    expect(screen.queryByTestId("detail-assignee-chip")).toBeNull();
  });

  it("renders the static fallback when there is no agent to watch", () => {
    render(<AssigneeChipView agent={null} watch={watchAt("working")} fallback={STATIC} />);
    expect(screen.getByTestId("static-fallback")).toBeInTheDocument();
    expect(screen.queryByTestId("detail-assignee-chip")).toBeNull();
  });
});
