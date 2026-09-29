import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { WorkingIndicators } from "@/components/discussion/WorkingIndicator";
import type { DispatchWatch } from "@/lib/discussion/working";

afterEach(cleanup);

function w(over: Partial<DispatchWatch> = {}): DispatchWatch {
  return {
    key: "m1:john",
    agentName: "john",
    messageId: "m1",
    phase: "working",
    since: 0,
    ...over,
  };
}

// ISI-5174: the room shows immediate + ongoing feedback that a background agent
// is working, and the eventual reply (or failure) lands visibly.

describe("WorkingIndicators", () => {
  it("renders nothing when there are no watches", () => {
    const { container } = render(<WorkingIndicators watches={[]} />);
    expect(container.firstChild).toBeNull();
    render(<WorkingIndicators />);
    expect(screen.queryByTestId("working-list")).toBeNull();
  });

  it("shows an animated working row for an active dispatch (AC1/AC2)", () => {
    render(<WorkingIndicators watches={[w()]} />);
    const row = screen.getByTestId("working-indicator");
    expect(row.getAttribute("data-phase")).toBe("working");
    expect(row.textContent).toContain("john is working");
    // The animated dots are present.
    expect(row.querySelectorAll(".ksq-working__dot")).toHaveLength(3);
  });

  it("shows a replied confirmation once the agent answers", () => {
    render(<WorkingIndicators watches={[w({ phase: "replied" })]} />);
    const row = screen.getByTestId("working-indicator");
    expect(row.getAttribute("data-phase")).toBe("replied");
    expect(row.textContent).toContain("john replied");
  });

  it("shows a failure state instead of silence (AC3)", () => {
    render(<WorkingIndicators watches={[w({ phase: "failed" })]} />);
    const row = screen.getByTestId("working-indicator");
    expect(row.getAttribute("data-phase")).toBe("failed");
    expect(row.textContent).toContain("could not respond");
  });

  it("announces politely via an aria-live status region", () => {
    render(<WorkingIndicators watches={[w()]} />);
    const list = screen.getByTestId("working-list");
    expect(list.getAttribute("aria-live")).toBe("polite");
    expect(list.getAttribute("role")).toBe("status");
  });

  it("renders one row per dispatched agent", () => {
    render(
      <WorkingIndicators
        watches={[
          w(),
          w({ key: "m1:bmad-pm", agentName: "bmad-pm", phase: "failed" }),
        ]}
      />,
    );
    const list = screen.getByTestId("working-list");
    expect(within(list).getAllByTestId("working-indicator")).toHaveLength(2);
  });
});
