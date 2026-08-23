import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { StatusChip } from "@/components/agents/StatusChip";

afterEach(cleanup);

// Story 8.10/8.11 at the component boundary: the derived status chip paints the four-value bucket
// with a status-keyed class (theme-invariant hue) and surfaces the paused sub-reason (story 7.6).

describe("<StatusChip>", () => {
  it("renders the status with a status-keyed class + data attribute", () => {
    render(<StatusChip status="running" />);
    const chip = screen.getByText("running");
    expect(chip).toHaveClass("agent-status--running");
    expect(chip).toHaveAttribute("data-status", "running");
  });

  it("surfaces the rate-limit paused sub-reason", () => {
    render(<StatusChip status="paused" pausedReason="rate_limited" />);
    expect(screen.getByText("paused: rate-limited")).toHaveClass(
      "agent-status--paused",
    );
  });

  it("renders a plain paused chip when there is no sub-reason", () => {
    render(<StatusChip status="paused" />);
    expect(screen.getByText("paused")).toBeInTheDocument();
  });
});
