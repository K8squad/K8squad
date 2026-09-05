// test/nav/SetupChip.test.tsx — the persistent "Finish setup (n/4)" chip (E1-S3 / ISI-3675,
// AC2/AC4, FR-1.3). Renders only while the journey is incomplete AND the Launchpad has been
// dismissed; routes to the next milestone's surface; gone at 4/4.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { SetupChip } from "@/components/nav/SetupChip";
import type { OnboardingProgress } from "@/lib/nav";

afterEach(cleanup);

const partial: OnboardingProgress = {
  step: 3,
  done: 2,
  total: 4,
  nextMilestone: "models",
  dismissed: true,
};

describe("SetupChip — AC2 (incomplete + dismissed → persistent chip)", () => {
  it("renders 'Finish setup (2/4)' routed to the next milestone's surface", () => {
    render(<SetupChip progress={partial} />);
    const chip = screen.getByTestId("setup-chip");
    expect(chip).toHaveTextContent("Finish setup (2/4)");
    expect(chip).toHaveAttribute("href", "/credentials");
    expect(chip).toHaveAttribute(
      "aria-label",
      "Finish setup, 2 of 4 milestones complete",
    );
  });

  it("routes milestone ids through the shared table (team → /compose, agents → /agents)", () => {
    const { unmount } = render(
      <SetupChip progress={{ ...partial, nextMilestone: "team", done: 0 }} />,
    );
    expect(screen.getByTestId("setup-chip")).toHaveAttribute("href", "/compose");
    unmount();
    render(<SetupChip progress={{ ...partial, nextMilestone: "agents", done: 1 }} />);
    expect(screen.getByTestId("setup-chip")).toHaveAttribute("href", "/agents");
  });
});

describe("SetupChip — hidden states", () => {
  it("stays hidden while the Launchpad has not been dismissed (AC2's Given)", () => {
    render(<SetupChip progress={{ ...partial, dismissed: false }} />);
    expect(screen.queryByTestId("setup-chip")).toBeNull();
  });

  it("disappears at 4/4 (AC4)", () => {
    render(
      <SetupChip progress={{ step: 4, done: 4, total: 4, dismissed: true }} />,
    );
    expect(screen.queryByTestId("setup-chip")).toBeNull();
  });

  it("fails open to nothing when the projection is unavailable", () => {
    render(<SetupChip progress={null} />);
    expect(screen.queryByTestId("setup-chip")).toBeNull();
  });
});
