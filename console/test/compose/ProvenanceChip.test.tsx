import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import {
  ProvenanceChip,
  provenanceLabel,
} from "@/components/compose/ProvenanceChip";

afterEach(cleanup);

// ISI-4893 / S4 at the component boundary: the provenance atom paints the winning tier with a
// tier-keyed class + data attribute (theme-invariant hue via globals.css tokens), degrades the
// role name fail-soft, and stays pure-presentational (title === label, decorative dot aria-hidden).

describe("<ProvenanceChip>", () => {
  it("AC1: renders the org-default tier", () => {
    render(<ProvenanceChip tier="default" />);
    const chip = screen.getByText("from Default");
    expect(chip).toHaveClass("provenance-chip--default");
    expect(chip).toHaveAttribute("data-tier", "default");
  });

  it("AC2: renders the role tier with a name", () => {
    render(<ProvenanceChip tier="role" roleName="implementer" />);
    const chip = screen.getByText("from Role: implementer");
    expect(chip).toHaveClass("provenance-chip--role");
    expect(chip).toHaveAttribute("data-tier", "role");
  });

  it("AC3: degrades to plain 'from Role' when no name is given (no 'undefined')", () => {
    render(<ProvenanceChip tier="role" />);
    expect(screen.getByText("from Role")).toBeInTheDocument();
    expect(screen.queryByText(/undefined/)).toBeNull();
  });

  it("AC4: renders the agent-override tier", () => {
    render(<ProvenanceChip tier="agent" />);
    const chip = screen.getByText("Agent override");
    expect(chip).toHaveClass("provenance-chip--agent");
    expect(chip).toHaveAttribute("data-tier", "agent");
  });

  it("AC6: title mirrors the visible label and the dot is decorative", () => {
    const { container } = render(
      <ProvenanceChip tier="role" roleName="implementer" />,
    );
    expect(screen.getByText("from Role: implementer")).toHaveAttribute(
      "title",
      "from Role: implementer",
    );
    const dot = container.querySelector(".provenance-chip__dot");
    expect(dot).toHaveAttribute("aria-hidden", "true");
  });

  it("provenanceLabel helper is reusable and fail-soft", () => {
    expect(provenanceLabel("default")).toBe("from Default");
    expect(provenanceLabel("role", "implementer")).toBe(
      "from Role: implementer",
    );
    expect(provenanceLabel("role")).toBe("from Role");
    expect(provenanceLabel("agent")).toBe("Agent override");
  });
});
