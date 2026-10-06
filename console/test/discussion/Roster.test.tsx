import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { Roster } from "@/components/discussion/Roster";

afterEach(cleanup);

// ISI-4929 (plan §4.5): the roster shows agent presence as
// online / working / offline, folded from the four-value agent status bucket.
// Display-only legibility — no custody verb rides this panel.

describe("<Roster> — presence projection", () => {
  it("renders one row per agent with the folded presence bucket", () => {
    render(
      <Roster
        agents={[
          { id: "a-1", name: "Amelia", status: "idle" },
          { id: "a-2", name: "Winston", status: "running" },
          { id: "a-3", name: "Parked", status: "paused" },
        ]}
      />,
    );
    const rows = screen.getAllByTestId("roster-agent");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveAttribute("data-presence", "online");
    expect(rows[1]).toHaveAttribute("data-presence", "working");
    expect(rows[2]).toHaveAttribute("data-presence", "offline");
  });

  it("keeps the known status text legible as the sub-label", () => {
    render(
      <Roster agents={[{ id: "a-2", name: "Winston", status: "running" }]} />,
    );
    expect(screen.getByText("running")).toBeTruthy();
  });

  it("an unknown/missing status degrades to offline + dash (no invented time)", () => {
    render(<Roster agents={[{ id: "a-4", name: "Ghost" }]} />);
    expect(screen.getByTestId("roster-agent")).toHaveAttribute(
      "data-presence",
      "offline",
    );
    expect(screen.getByText("—")).toBeTruthy();
  });

  it("an empty roster renders the empty state, not a broken panel", () => {
    render(<Roster agents={[]} />);
    expect(screen.getByTestId("roster-empty")).toBeTruthy();
    expect(screen.queryByTestId("roster-agent")).toBeNull();
  });
});

// ISI-5527 (parent ISI-5520): the row visibly flips idle → queued → running from
// the per-agent live-run map (lib/overview/liveRunsByAgent), driven by the polling
// squad/overview feed. idle stays the plain presence row; running/queued paint the
// blue pulse dot + chip + ticket-ref sub-label, announced via aria-live=polite.
describe("<Roster> — live-run flip", () => {
  const agents = [
    { id: "amelia", name: "Amelia", status: "idle" },
    { id: "winston", name: "Winston", status: "idle" },
  ];

  it("paints the running treatment with the chip + 'working a run · <ticket>' sub-label", () => {
    render(
      <Roster
        agents={agents}
        liveRuns={{
          amelia: { state: "running", phase: "Running", workItem: "ISI-42" },
        }}
      />,
    );
    const rows = screen.getAllByTestId("roster-agent");
    // Amelia is running; Winston (absent from the map) stays idle.
    expect(rows[0]).toHaveAttribute("data-run-state", "running");
    expect(rows[1]).toHaveAttribute("data-run-state", "idle");
    const chips = screen.getAllByTestId("roster-run-chip");
    expect(chips[0].textContent).toBe("running");
    expect(screen.getByTestId("roster-run-sublabel").textContent).toBe(
      "working a run · ISI-42",
    );
  });

  it("falls back to 'working a run' (no ticket) when the run carries no work item", () => {
    render(
      <Roster
        agents={agents}
        liveRuns={{ amelia: { state: "running", phase: "Running" } }}
      />,
    );
    expect(screen.getByTestId("roster-run-sublabel").textContent).toBe(
      "working a run",
    );
  });

  it("shows the honest 'queued' chip for a queued run (OQ4)", () => {
    render(
      <Roster
        agents={agents}
        liveRuns={{ winston: { state: "queued", phase: "Pending" } }}
      />,
    );
    const rows = screen.getAllByTestId("roster-agent");
    expect(rows[1]).toHaveAttribute("data-run-state", "queued");
    expect(screen.getByTestId("roster-run-chip").textContent).toBe("queued");
  });

  it("idle → running → idle: re-render with/without the map flips the row back", () => {
    const { rerender } = render(<Roster agents={agents} liveRuns={{}} />);
    expect(screen.getAllByTestId("roster-agent")[0]).toHaveAttribute(
      "data-run-state",
      "idle",
    );
    rerender(
      <Roster
        agents={agents}
        liveRuns={{ amelia: { state: "running", phase: "Running", workItem: "ISI-9" } }}
      />,
    );
    expect(screen.getAllByTestId("roster-agent")[0]).toHaveAttribute(
      "data-run-state",
      "running",
    );
    // Run ends → the agent drops out of the overview map → back to the plain row.
    rerender(<Roster agents={agents} liveRuns={{}} />);
    expect(screen.getAllByTestId("roster-agent")[0]).toHaveAttribute(
      "data-run-state",
      "idle",
    );
    expect(screen.queryByTestId("roster-run-chip")).toBeNull();
  });

  it("announces live agents in a polite region (aria-live mechanism)", () => {
    render(
      <Roster
        agents={agents}
        liveRuns={{
          amelia: { state: "running", phase: "Running", workItem: "ISI-42" },
          winston: { state: "queued", phase: "Pending" },
        }}
      />,
    );
    const region = screen.getByTestId("roster-live-region");
    expect(region).toHaveAttribute("aria-live", "polite");
    expect(region.textContent).toBe("Amelia is running. Winston is queued");
  });

  it("an empty live-run map leaves every row idle (graceful, no chips)", () => {
    render(<Roster agents={agents} liveRuns={{}} />);
    expect(screen.queryByTestId("roster-run-chip")).toBeNull();
    for (const row of screen.getAllByTestId("roster-agent")) {
      expect(row).toHaveAttribute("data-run-state", "idle");
    }
  });
});
