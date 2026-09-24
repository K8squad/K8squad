// test/compose/EffectiveModelReadout.test.tsx — the S3 effective-model read-out
// at the component boundary (ISI-4892, epic ISI-4822 Flow C).
//
// Locks the read-only projection contract: the widget fetches the BFF effective-
// model route, renders the resolved model + the S4 <ProvenanceChip> keyed by the
// SERVER-returned tier (it re-derives no precedence), shows the fail-closed
// guardrail on unresolved, and hides entirely in create mode / on 404. It never
// renders an input or a write affordance.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { EffectiveModelReadout } from "@/components/compose/EffectiveModelReadout";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("<EffectiveModelReadout>", () => {
  it("hides in create mode (no persisted agent name) and issues no fetch", () => {
    const { container } = render(<EffectiveModelReadout />);
    expect(container).toBeEmptyDOMElement();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("AC2: shows the inherited role model with a violet 'from Role' chip + the read-only helper", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ model: "claude-sonnet-4-5", tier: "role", roleName: "implementer" }),
    );
    render(<EffectiveModelReadout agentName="cade" />);

    await waitFor(() => expect(screen.getByText("claude-sonnet-4-5")).toBeInTheDocument());
    const chip = screen.getByText("from Role: implementer");
    expect(chip).toHaveClass("provenance-chip--role");
    expect(
      screen.getByText(/Set a model above to override at the Agent tier/),
    ).toBeInTheDocument();
    // one read, keyed by the agent name; GET-only (no write verb / body).
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toContain("/api/squad/agents/cade/effective-model");
  });

  it("AC2: shows the org-default model with a blue 'from Default' chip", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model: "claude-opus-5", tier: "default" }));
    render(<EffectiveModelReadout agentName="cade" />);

    await waitFor(() => expect(screen.getByText("claude-opus-5")).toBeInTheDocument());
    expect(screen.getByText("from Default")).toHaveClass("provenance-chip--default");
  });

  it("AC3: shows an Agent-override model with a green chip and NO inherit helper", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ model: "gpt-5", tier: "agent", roleName: "implementer" }),
    );
    render(<EffectiveModelReadout agentName="cade" />);

    await waitFor(() => expect(screen.getByText("gpt-5")).toBeInTheDocument());
    expect(screen.getByText("Agent override")).toHaveClass("provenance-chip--agent");
    expect(screen.queryByText(/Set a model above to override/)).toBeNull();
  });

  it("AC4: renders the fail-closed guardrail (no invented model) on unresolved", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ unresolved: true, roleName: "implementer" }));
    const { container } = render(<EffectiveModelReadout agentName="stranded" />);

    await waitFor(() =>
      expect(screen.getByText(/No effective model — the org default is unset/)).toBeInTheDocument(),
    );
    // no chip, no fabricated model id.
    expect(container.querySelector(".provenance-chip")).toBeNull();
    expect(container.querySelector(".effective-model--unresolved")).not.toBeNull();
  });

  it("hides on a 404 (unpersisted / out-of-scope) rather than alarming", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ error: "no such agent" }, 404));
    const { container } = render(<EffectiveModelReadout agentName="ghost" />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(container).toBeEmptyDOMElement();
  });

  it("is read-only — renders no input, textarea, or button", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model: "claude-sonnet-4-5", tier: "role", roleName: "r" }));
    const { container } = render(<EffectiveModelReadout agentName="cade" />);
    await waitFor(() => expect(screen.getByText("claude-sonnet-4-5")).toBeInTheDocument());
    expect(container.querySelector("input,textarea,button,select")).toBeNull();
  });

  it("passes the ?team= act-as-team selector through to the read route", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model: "m", tier: "role", roleName: "r" }));
    render(<EffectiveModelReadout agentName="cade" team="team-beta" />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(fetchMock.mock.calls[0][0]).toContain("team=team-beta");
  });
});
