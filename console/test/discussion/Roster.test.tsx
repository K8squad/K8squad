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
