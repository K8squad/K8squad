// test/agents/agentEditAction.test.tsx — the "Edit" deep-link on agent detail (ISI-3554 Story A).
//
// Proves AC3 (Edit deep-links into compose edit mode with the agent name pre-filled) and AC5 (the
// action is gated: absent when the caller can't compose). It renders the real AgentDetail, stubs the
// two BFF reads it makes (/api/agents/{id}, /api/agents/{id}/runs), and asserts the Edit anchor's
// href — the exact URL contract ComposeScreen consumes back.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import { AgentDetail } from "@/components/agents/AgentDetail";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const AGENT = {
  id: "a-1",
  name: "backup-architect",
  runtimeType: "openclaw",
  status: "idle",
  roles: [{ id: "r-1", name: "Architect" }],
};

/** Stub the two AgentDetail reads; the runs list is empty (the header is what we assert on). */
function stubAgent() {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string) => {
      const url = String(input);
      const body = url.includes("/runs") ? [] : AGENT;
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
      });
    }),
  );
}

describe("AgentDetail Edit deep-link", () => {
  it("renders an Edit action linking to compose edit mode with the name pre-filled (AC3)", async () => {
    stubAgent();
    render(<AgentDetail agentId="a-1" canEdit />);
    const edit = await screen.findByRole("link", { name: "Edit backup-architect" });
    expect(edit).toHaveAttribute(
      "href",
      "/compose?kind=agents&mode=edit&name=backup-architect",
    );
  });

  it("hides the Edit action when the caller cannot compose (AC5)", async () => {
    stubAgent();
    render(<AgentDetail agentId="a-1" canEdit={false} />);
    // Wait for the agent header to render, then assert no Edit affordance is present.
    await waitFor(() => expect(screen.getByText("backup-architect")).toBeInTheDocument());
    expect(screen.queryByRole("link", { name: /^Edit / })).toBeNull();
  });

  it("defaults to no Edit action when canEdit is omitted (fails closed)", async () => {
    stubAgent();
    render(<AgentDetail agentId="a-1" />);
    await waitFor(() => expect(screen.getByText("backup-architect")).toBeInTheDocument());
    expect(screen.queryByRole("link", { name: /^Edit / })).toBeNull();
  });
});
