import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { AuthorBadge } from "@/components/discussion/AuthorBadge";
import { deriveAuthorBadge } from "@/lib/discussion/provenance";

afterEach(cleanup);

// AC2 at the component boundary: every message renders a badge; Run chip
// deep-links to 8.11; a defect renders a visible "unattributed" marker.

describe("<AuthorBadge> — AC2 rendering", () => {
  it("renders an agent name", () => {
    render(
      <AuthorBadge
        badge={deriveAuthorBadge({
          authorType: "agent",
          authorName: "planner-1",
          metadata: null,
        })}
      />,
    );
    const chip = screen.getByTestId("author-chip");
    expect(chip).toHaveAttribute("data-kind", "agent");
    expect(chip).toHaveTextContent("planner-1");
    expect(screen.queryByTestId("run-chip")).toBeNull();
  });

  it("renders a Run chip deep-linking to the Run detail (8.11)", () => {
    render(
      <AuthorBadge
        badge={deriveAuthorBadge({
          authorType: "agent",
          authorName: "coder-2",
          metadata: { runId: "run-123" },
        })}
      />,
    );
    const run = screen.getByTestId("run-chip");
    expect(run).toHaveAttribute("href", "/runs/run-123");
    expect(run).toHaveAttribute("data-run-id", "run-123");
  });

  it("renders a visible 'unattributed' marker for a provenance defect (never blank)", () => {
    render(
      <AuthorBadge
        badge={deriveAuthorBadge({
          authorType: "" as unknown as "agent",
          authorName: "",
          metadata: null,
        })}
      />,
    );
    const chip = screen.getByTestId("author-chip");
    expect(chip).toHaveAttribute("data-defect", "true");
    expect(chip).toHaveTextContent("unattributed");
  });

  it("carries its base colour as --chip-base so the border derives from base (8.9)", () => {
    render(
      <AuthorBadge
        badge={deriveAuthorBadge({
          authorType: "human",
          authorName: "u",
          metadata: null,
        })}
      />,
    );
    const chip = screen.getByTestId("author-chip");
    // The theme-invariant border is finalized in CSS from --chip-base; assert
    // the human base token is what the chip carries (never a hardcoded colour).
    expect((chip as HTMLElement).style.getPropertyValue("--chip-base")).toBe(
      "var(--ksq-badge-human)",
    );
  });
});
