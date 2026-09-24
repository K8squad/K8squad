// test/tickets/DispatchPendingCard.test.tsx — ISI-4880 (S2 of ISI-4853).
//
// The card is PURE presentational: we feed it the S1 hook's `DispatchWatch` output verbatim and assert
// it paints the honest ladder (§3) — correct label/tone/motion per state, no layout jump (same shell as
// RunCommentCard), a role="status" live label, and reduced-motion respected via the reused CSS-gated
// animation classes (there is no JS-driven motion to disable).

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { DispatchPendingCard } from "@/components/tickets/DispatchPendingCard";
import { toDispatchWatch, type DispatchWatch } from "@/lib/tickets/useDispatchWatch";

afterEach(cleanup);

// Build a DispatchWatch straight off the machine so the test consumes the SAME contract the hook emits
// (label/tone are derived there — this keeps the card test honest to the real ladder).
function watchFor(
  state: DispatchWatch["state"],
  degrade: DispatchWatch["degrade"] = "none",
): DispatchWatch {
  return toDispatchWatch({ state, degrade });
}

describe("<DispatchPendingCard>", () => {
  // ── §3 label + tone per ladder state ──────────────────────────────────────────────────────────
  const cases: Array<{
    state: DispatchWatch["state"];
    label: string;
    tone: string;
    motion: string;
  }> = [
    { state: "queued", label: "Queued", tone: "idle", motion: "static" },
    { state: "picking_up", label: "Picking up…", tone: "pending", motion: "spinner" },
    { state: "working", label: "Working…", tone: "running", motion: "pulse" },
    { state: "succeeded", label: "Finished", tone: "running", motion: "static" },
    { state: "failed", label: "Failed", tone: "blocked", motion: "static" },
  ];

  for (const c of cases) {
    it(`AC1: ${c.state} → label "${c.label}", tone ${c.tone}, ${c.motion} motion`, () => {
      render(<DispatchPendingCard watch={watchFor(c.state)} agentName="Claude Code" />);

      expect(screen.getByTestId("dispatch-label")).toHaveTextContent(c.label);

      const card = screen.getByTestId("dispatch-pending-card");
      expect(card).toHaveAttribute("data-state", c.state);
      expect(card).toHaveAttribute("data-tone", c.tone);

      const status = screen.getByTestId("dispatch-status");
      expect(status).toHaveAttribute("data-tone", c.tone);

      expect(screen.getByTestId("dispatch-indicator")).toHaveAttribute("data-motion", c.motion);
    });
  }

  // ── Honesty guard at the card boundary: "Working…" only ever renders for the working state ─────
  it("AC1: never labels a non-working state 'Working…' (honesty guard)", () => {
    for (const c of cases.filter((x) => x.state !== "working")) {
      cleanup();
      render(<DispatchPendingCard watch={watchFor(c.state)} agentName="Claude Code" />);
      expect(screen.queryByText("Working…")).toBeNull();
    }
  });

  // ── Motion reuse (§ zero new tokens / reduced-motion) ─────────────────────────────────────────
  it("AC1: picking_up reuses the shared .rail__spinner (amber via reused token, not a new one)", () => {
    render(<DispatchPendingCard watch={watchFor("picking_up")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-indicator")).toHaveClass("rail__spinner");
  });

  it("AC1: working reuses the existing run pulse (ksq-runcomment__dot--live)", () => {
    render(<DispatchPendingCard watch={watchFor("working")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-indicator")).toHaveClass("ksq-runcomment__dot--live");
  });

  // ── AC2: no layout jump — the placeholder shares RunCommentCard's outer shell so the real card
  //         mutates in place at the same slot. ──────────────────────────────────────────────────
  it("AC2: renders the RunCommentCard shell so the real card can replace it with no shift", () => {
    render(<DispatchPendingCard watch={watchFor("queued")} agentName="Claude Code" />);
    const card = screen.getByTestId("dispatch-pending-card");
    expect(card.tagName).toBe("LI");
    // Same shell classes RunCommentCard uses → identical box model at the slot.
    expect(card).toHaveClass("ksq-activity", "ksq-activity--comment", "ksq-runcomment");
    // Avatar spine + bubble present, matching the run card anatomy.
    expect(card.querySelector(".ksq-runcomment__avatar")).not.toBeNull();
    expect(card.querySelector(".ksq-runcomment__bubble")).not.toBeNull();
  });

  it("AC2: the shell stays stable across every ladder state (no structural jump)", () => {
    const shells = cases.map((c) => {
      cleanup();
      const { container } = render(
        <DispatchPendingCard watch={watchFor(c.state)} agentName="Claude Code" />,
      );
      const li = container.querySelector("li")!;
      // Snapshot the structural skeleton (tag + shell classes + child anatomy), which must NOT vary.
      return [
        li.tagName,
        li.classList.contains("ksq-activity"),
        li.classList.contains("ksq-runcomment"),
        !!li.querySelector(".ksq-runcomment__avatar"),
        !!li.querySelector(".ksq-runcomment__bubble"),
        !!li.querySelector(".ksq-dispatch-card__skeleton"),
      ].join("|");
    });
    // Every state produces the identical structural signature → the card mutates in place.
    expect(new Set(shells).size).toBe(1);
  });

  // ── AC3: role="status" + accessible live label ────────────────────────────────────────────────
  it("AC3: exposes role=status with an accessible, honest live label", () => {
    render(<DispatchPendingCard watch={watchFor("picking_up")} agentName="Claude Code" />);
    const card = screen.getByRole("status");
    expect(card).toHaveAttribute("aria-live", "polite");
    expect(card).toHaveAttribute("aria-label", "Run status: Picking up…");
    // Decorative motion element is hidden from the a11y tree.
    expect(screen.getByTestId("dispatch-indicator")).toHaveAttribute("aria-hidden", "true");
  });

  // ── AC3: reduced-motion — motion is 100% CSS via reused, already-gated animation classes, so there
  //         is nothing JS to toggle; assert the animated elements carry those gated classes. ──────
  it("AC3: all motion rides reduced-motion-gated CSS classes (no un-gated JS animation)", () => {
    // queued: skeleton shimmer (ksq-skel-bar is gated in tickets.css @prefers-reduced-motion)
    render(<DispatchPendingCard watch={watchFor("queued")} agentName="Claude Code" />);
    expect(screen.getAllByTestId("dispatch-skeleton")[0].querySelector(".ksq-skel-bar")).not.toBeNull();
    cleanup();
    // picking_up: .rail__spinner (gated in globals.css)
    render(<DispatchPendingCard watch={watchFor("picking_up")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-indicator")).toHaveClass("rail__spinner");
    cleanup();
    // working: ksq-runcomment__dot--live (gated in tickets.css)
    render(<DispatchPendingCard watch={watchFor("working")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-indicator")).toHaveClass("ksq-runcomment__dot--live");
  });

  // ── Degrade note (design §4) — queued-only, honest copy ───────────────────────────────────────
  it("shows no degrade note while queued with degrade=none", () => {
    render(<DispatchPendingCard watch={watchFor("queued", "none")} agentName="Claude Code" />);
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();
  });

  it("shows the soft note at soft degrade (still queued)", () => {
    render(<DispatchPendingCard watch={watchFor("queued", "soft")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-degrade")).toHaveTextContent(/usually takes a few seconds/);
  });

  it("shows the stalled note at stalled degrade (still queued)", () => {
    render(<DispatchPendingCard watch={watchFor("queued", "stalled")} agentName="Claude Code" />);
    expect(screen.getByTestId("dispatch-degrade")).toHaveTextContent(/re-assign/);
  });

  it("never shows a degrade note once past queued (degrade is meaningless there)", () => {
    // The hook clears degrade on advance, but defend at the card too.
    render(
      <DispatchPendingCard
        watch={{ state: "working", label: "Working…", tone: "running", degrade: "stalled" }}
        agentName="Claude Code"
      />,
    );
    expect(screen.queryByTestId("dispatch-degrade")).toBeNull();
  });

  it("renders the agent header name + avatar glyph", () => {
    render(<DispatchPendingCard watch={watchFor("queued")} agentName="Claude Code" avatar="C" />);
    expect(screen.getByText("Claude Code")).toBeInTheDocument();
    expect(screen.getByText("C")).toBeInTheDocument();
  });
});
