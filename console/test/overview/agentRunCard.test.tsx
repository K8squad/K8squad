import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { AgentRunCard, AgentRunCardGrid } from "@/components/overview/AgentRunCard";
import {
  agentInitials,
  avatarColor,
  relativeActivityShort,
  resolveCardState,
  resolveDeepLink,
  thinkingLine,
  WAITING_SNIPPET,
  UNAVAILABLE,
  type AgentActivity,
} from "@/lib/overview/agentActivity";

afterEach(cleanup);

// ISI-5707 WS-2 — the Paperclip-style agent card (DESIGN ISI-5699 mock B). Coverage focuses on the
// contract points: the five states (running/idle/blocked/empty/loading), FR-I3 fabrication
// discipline (— / waiting for first step… / no dead chip), classified-snippet-only rendering, the
// work-item-vs-discussion deep-link chip, and the semantic (no-new-token) phase-chip tone.

const NOW = Date.parse("2026-10-12T18:00:00Z");

function agent(over: Partial<AgentActivity> = {}): AgentActivity {
  return {
    agentId: "winston-uid",
    name: "Winston",
    role: "DevOps",
    state: "running",
    currentOrLastRun: {
      runId: "intake-abc12345-r7",
      phase: "Running",
      workItemRef: "ISI-5687",
      workItemUrl: "/projects/k8squad/issues/isi-5687",
      lastThinkingSnippet: "helm upgrade: pinning the operator.sandbox.image digest",
      lastActivityAt: "2026-10-12T18:00:00Z",
    },
    ...over,
  };
}

describe("agentActivity projections", () => {
  it("resolveCardState: loading > empty > aggregate state", () => {
    expect(resolveCardState(agent(), true)).toBe("loading");
    expect(resolveCardState(null)).toBe("loading");
    expect(resolveCardState(agent({ currentOrLastRun: null }))).toBe("empty");
    expect(resolveCardState(agent({ state: "blocked" }))).toBe("blocked");
  });

  it("agentInitials handles one- and multi-word names", () => {
    expect(agentInitials("Winston")).toBe("WI");
    expect(agentInitials("Claude Code")).toBe("CC");
    expect(agentInitials("code-reviewer")).toBe("CR");
    expect(agentInitials("")).toBe("?");
  });

  it("avatarColor is deterministic per seed", () => {
    expect(avatarColor("winston-uid")).toBe(avatarColor("winston-uid"));
    expect(avatarColor("winston-uid")).not.toBe(avatarColor("alfred-uid"));
  });

  it("thinkingLine applies FR-I3: snippet, else waiting (running), else em-dash", () => {
    const run = agent().currentOrLastRun!;
    expect(thinkingLine("running", run)).toBe(run.lastThinkingSnippet);
    expect(thinkingLine("running", { ...run, lastThinkingSnippet: null })).toBe(WAITING_SNIPPET);
    expect(thinkingLine("idle", { ...run, lastThinkingSnippet: "  " })).toBe(UNAVAILABLE);
  });

  it("resolveDeepLink prefers ticket, falls back to discussion, else null", () => {
    expect(resolveDeepLink(agent().currentOrLastRun)).toEqual({
      href: "/projects/k8squad/issues/isi-5687",
      label: "ISI-5687",
      kind: "ticket",
    });
    expect(
      resolveDeepLink({
        runId: "r1",
        phase: "Running",
        workItemUrl: null,
        discussionUrl: "/projects/k8squad/discussions/d1",
      }),
    ).toEqual({ href: "/projects/k8squad/discussions/d1", label: "discussion", kind: "discussion" });
    expect(resolveDeepLink({ runId: "r1", phase: "Running" })).toBeNull();
  });

  it("relativeActivityShort: now / m / h / d ago, em-dash when absent", () => {
    expect(relativeActivityShort("2026-10-12T18:00:00Z", NOW)).toBe("now");
    expect(relativeActivityShort("2026-10-12T17:38:00Z", NOW)).toBe("22m ago");
    expect(relativeActivityShort("2026-10-12T15:00:00Z", NOW)).toBe("3h ago");
    expect(relativeActivityShort(null, NOW)).toBe(UNAVAILABLE);
    expect(relativeActivityShort("not-a-date", NOW)).toBe(UNAVAILABLE);
  });
});

describe("<AgentRunCard>", () => {
  it("running: name/role, running chip (running tone), classified thinking, ticket chip, time", () => {
    render(<AgentRunCard agent={agent()} now={NOW} />);
    const card = screen.getByTestId("agent-run-card");
    expect(card).toHaveAttribute("data-state", "running");
    expect(within(card).getByText("Winston")).toBeInTheDocument();
    // Role renders as a scannable tag (ISI-5753), not a bare sub-label.
    const roleTag = within(card).getByTestId("agent-role");
    expect(roleTag).toHaveTextContent("DevOps");
    expect(roleTag).toHaveClass("ov-agentcard__role");
    expect(roleTag.querySelector("svg")).toBeInTheDocument();
    const chip = screen.getByTestId("agent-phase-chip");
    expect(chip).toHaveClass("ov-tone--running");
    expect(chip).toHaveTextContent("running");
    expect(screen.getByTestId("agent-thinking")).toHaveTextContent(
      "helm upgrade: pinning the operator.sandbox.image digest",
    );
    const chipLink = screen.getByTestId("agent-workchip");
    expect(chipLink).toHaveAttribute("href", "/projects/k8squad/issues/isi-5687");
    expect(chipLink).toHaveTextContent("ISI-5687");
    expect(screen.getByTestId("agent-time")).toHaveTextContent("now");
  });

  it("omits the role tag entirely when the agent has no role (no empty pill)", () => {
    render(<AgentRunCard agent={agent({ role: null })} now={NOW} />);
    expect(screen.queryByTestId("agent-role")).not.toBeInTheDocument();
  });

  it("idle: shows the last run with the idle tone", () => {
    render(
      <AgentRunCard
        agent={agent({ state: "idle", name: "Alfred", role: "CTO" })}
        now={NOW}
      />,
    );
    expect(screen.getByTestId("agent-run-card")).toHaveAttribute("data-state", "idle");
    expect(screen.getByTestId("agent-phase-chip")).toHaveClass("ov-tone--idle");
  });

  it("blocked: red (blocked) tone; the blocking reason rides the thinking line", () => {
    render(
      <AgentRunCard
        agent={agent({
          state: "blocked",
          name: "Code Reviewer",
          role: "Reviewer",
          currentOrLastRun: {
            runId: "r3",
            phase: "Blocked",
            workItemRef: "ISI-5694",
            workItemUrl: "/projects/k8squad/issues/isi-5694",
            lastThinkingSnippet: "Waiting on CI: FR-B3 allowlist gate",
            lastActivityAt: "2026-10-12T17:49:00Z",
          },
        })}
        now={NOW}
      />,
    );
    expect(screen.getByTestId("agent-phase-chip")).toHaveClass("ov-tone--blocked");
    expect(screen.getByTestId("agent-thinking")).toHaveTextContent("Waiting on CI: FR-B3 allowlist gate");
    expect(screen.getByTestId("agent-time")).toHaveTextContent("11m ago");
  });

  it("empty: no chip, no dead work-chip, honest 'No run yet' line", () => {
    render(<AgentRunCard agent={agent({ name: "Scribe", role: "Docs", currentOrLastRun: null })} now={NOW} />);
    const card = screen.getByTestId("agent-run-card");
    expect(card).toHaveAttribute("data-state", "empty");
    expect(screen.queryByTestId("agent-phase-chip")).not.toBeInTheDocument();
    expect(screen.queryByTestId("agent-workchip")).not.toBeInTheDocument();
    expect(screen.getByTestId("agent-empty")).toHaveTextContent("No run yet — idle since onboarding");
  });

  it("running with no snippet yet renders the waiting line, never a blank (FR-I3)", () => {
    render(
      <AgentRunCard
        agent={agent({ currentOrLastRun: { runId: "r9", phase: "Claiming", lastActivityAt: "2026-10-12T18:00:00Z" } })}
        now={NOW}
      />,
    );
    expect(screen.getByTestId("agent-thinking")).toHaveTextContent(WAITING_SNIPPET);
    // no ticket/discussion → no dead link, an explicit em-dash placeholder instead
    expect(screen.queryByTestId("agent-workchip")).not.toBeInTheDocument();
  });

  it("loading: skeleton only, no agent text, no spinner-blank", () => {
    const { container } = render(<AgentRunCard loading />);
    expect(screen.queryByTestId("agent-run-card")).not.toBeInTheDocument();
    expect(container.querySelector(".ov-agentcard--loading")).toBeInTheDocument();
    expect(container.querySelectorAll(".ov-skel").length).toBeGreaterThan(0);
  });

  it("never renders the raw [run]/[tool] execution stream verbatim (snippet is pre-classified)", () => {
    // The card shows whatever WS-1a already classified. It must not itself surface raw markers,
    // and the thinking text comes straight from lastThinkingSnippet — no raw prefix is injected.
    render(<AgentRunCard agent={agent()} now={NOW} />);
    expect(screen.getByTestId("agent-thinking").textContent).not.toMatch(/\[run|\[tool|\[result/);
  });

  it("AgentRunCardGrid wraps cards in the responsive grid", () => {
    const { container } = render(
      <AgentRunCardGrid>
        <AgentRunCard agent={agent()} now={NOW} />
        <AgentRunCard agent={agent({ agentId: "a2", name: "Alfred" })} now={NOW} />
      </AgentRunCardGrid>,
    );
    expect(container.querySelector(".ov-agentcard-grid")).toBeInTheDocument();
    expect(container.querySelectorAll(".ov-agentcard").length).toBe(2);
  });
});
