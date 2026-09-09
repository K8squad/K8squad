// test/compose/composeScreen.delete.test.tsx — Compose destructive-delete UI (ISI-4108).
//
// Frontend for ISI-4107's Agent + Project DELETE path. Four seams land here:
//  1. Confirm-gate: opening the dialog does NOT fire a DELETE; only the confirm button does.
//  2. 204 No Content → the left-pane list refetches and the form resets to a fresh create form.
//  3. A denial/error (403/409/…) is surfaced VERBATIM through the existing ErrorBanner.
//  4. Agent delete carries the project RBAC scope as ?project=; Project delete carries no query.
//     Team delete is intentionally absent (board-gated on the parent ISI-4107).

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { ComposeScreen } from "@/components/compose/ComposeScreen";

let current = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useSearchParams: () => current,
}));

type Route = { status: number; body?: unknown };
interface Call {
  url: string;
  method: string;
}

/**
 * URL-aware fetch stub that also records (url, method) per call. First matching prefix wins;
 * unmatched URLs 404. Later routes can be more specific than earlier ones by ordering keys longest
 * first. 204 responses carry an empty body.
 */
function stubRoutes(routes: Record<string, Route>): { spy: ReturnType<typeof vi.fn>; calls: Call[] } {
  const calls: Call[] = [];
  const keys = Object.keys(routes).sort((a, b) => b.length - a.length);
  const spy = vi.fn((input: string, init?: { method?: string }) => {
    const url = String(input);
    const method = (init?.method ?? "GET").toUpperCase();
    calls.push({ url, method });
    const key = keys.find((k) => url.startsWith(k));
    const r = key ? routes[key] : { status: 404 };
    const ok = r.status >= 200 && r.status < 300;
    return Promise.resolve({
      ok,
      status: r.status,
      text: () => Promise.resolve(r.body === undefined ? "" : JSON.stringify(r.body)),
      json: () => Promise.resolve(r.body ?? null),
    });
  });
  vi.stubGlobal("fetch", spy);
  return { spy, calls };
}

const tenantTeams = {
  fleet: false,
  teams: [{ name: "own", namespace: "own-ns", uid: "u-own", agentCount: 1, projectCount: 1 }],
};

beforeEach(() => {
  current = new URLSearchParams();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** Deep-link into edit mode on an existing agent and wait for the Delete affordance to render. */
async function renderAgentEdit(extra: Record<string, Route> = {}) {
  current = new URLSearchParams("kind=agents&mode=edit&name=my-agent");
  const { calls } = stubRoutes({
    "/api/squad/teams": { status: 200, body: tenantTeams },
    "/api/squad/agents/my-agent": { status: 200, body: { name: "my-agent" } },
    "/api/squad/agents": { status: 200, body: { agents: [{ name: "my-agent", runtime: "claude" }] } },
    ...extra,
  });
  render(<ComposeScreen />);
  const openBtn = await screen.findByTestId("compose-delete-open");
  await waitFor(() => expect(openBtn).not.toBeDisabled());
  return { calls };
}

describe("Compose delete UI — confirm gate (ISI-4108)", () => {
  it("opening the dialog fires NO DELETE until the operator confirms", async () => {
    const { calls } = await renderAgentEdit();

    fireEvent.click(screen.getByTestId("compose-delete-open"));
    // Dialog is up…
    expect(await screen.findByTestId("compose-delete-confirm")).toBeInTheDocument();
    // …but nothing was deleted yet.
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);

    // Cancelling still fires no DELETE and dismisses the dialog.
    fireEvent.click(screen.getByTestId("compose-delete-cancel"));
    await waitFor(() =>
      expect(screen.queryByTestId("compose-delete-confirm")).not.toBeInTheDocument(),
    );
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
  });

  it("dialog is a labelled modal dismissable with Esc", async () => {
    await renderAgentEdit();
    fireEvent.click(screen.getByTestId("compose-delete-open"));
    const dialog = await screen.findByTestId("compose-delete-confirm");
    expect(dialog).toHaveAttribute("role", "dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAttribute("aria-labelledby", "compose-confirm-title");

    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByTestId("compose-delete-confirm")).not.toBeInTheDocument(),
    );
  });
});

describe("Compose delete UI — 204 success (ISI-4108)", () => {
  it("204 → refetches the left-pane list and resets to a create form", async () => {
    const { calls } = await renderAgentEdit({
      "/api/compose/agents/my-agent": { status: 204 },
    });

    const listCallsBefore = calls.filter((c) => c.url === "/api/squad/agents").length;

    fireEvent.click(screen.getByTestId("compose-delete-open"));
    fireEvent.click(await screen.findByTestId("compose-delete-confirm-btn"));

    // DELETE fired against the BFF compose route.
    await waitFor(() =>
      expect(calls.some((c) => c.method === "DELETE" && c.url.startsWith("/api/compose/agents/my-agent"))).toBe(true),
    );
    // Dialog closed, form dropped back to CREATE mode (button label flips), list refetched.
    await waitFor(() =>
      expect(screen.queryByTestId("compose-delete-confirm")).not.toBeInTheDocument(),
    );
    await waitFor(() => expect(screen.getByText("Create Agent")).toBeInTheDocument());
    await waitFor(() =>
      expect(calls.filter((c) => c.url === "/api/squad/agents").length).toBeGreaterThan(listCallsBefore),
    );
  });
});

describe("Compose delete UI — error surfacing (ISI-4108)", () => {
  it("a 403 denial is relayed verbatim via ErrorBanner", async () => {
    await renderAgentEdit({
      "/api/compose/agents/my-agent": { status: 403, body: { error: "forbidden: not a member" } },
    });

    fireEvent.click(screen.getByTestId("compose-delete-open"));
    fireEvent.click(await screen.findByTestId("compose-delete-confirm-btn"));

    expect(await screen.findByText("HTTP 403")).toBeInTheDocument();
    expect(screen.getByText("forbidden: not a member")).toBeInTheDocument();
    // Confirm dialog closed on error.
    expect(screen.queryByTestId("compose-delete-confirm")).not.toBeInTheDocument();
  });
});

describe("Compose delete UI — RBAC scope query (ISI-4108)", () => {
  it("agent delete carries ?project= from the form's project scope", async () => {
    const { calls } = await renderAgentEdit({
      "/api/compose/agents/my-agent": { status: 204 },
    });

    // The operator supplies the project RBAC scope (not persisted in the CRD read).
    fireEvent.change(screen.getByLabelText(/Project/i), { target: { value: "proj-x" } });
    fireEvent.click(screen.getByTestId("compose-delete-open"));
    fireEvent.click(await screen.findByTestId("compose-delete-confirm-btn"));

    await waitFor(() => {
      const del = calls.find((c) => c.method === "DELETE");
      expect(del).toBeDefined();
      expect(del!.url).toContain("project=proj-x");
    });
  });

  it("project delete carries no query (scope is its own name)", async () => {
    current = new URLSearchParams("kind=projects&mode=edit&name=my-proj");
    const { calls } = stubRoutes({
      "/api/squad/teams": { status: 200, body: tenantTeams },
      "/api/squad/projects/my-proj": { status: 200, body: { name: "my-proj" } },
      "/api/compose/projects/my-proj": { status: 204 },
    });
    render(<ComposeScreen />);
    const openBtn = await screen.findByTestId("compose-delete-open");
    await waitFor(() => expect(openBtn).not.toBeDisabled());

    fireEvent.click(openBtn);
    fireEvent.click(await screen.findByTestId("compose-delete-confirm-btn"));

    await waitFor(() => {
      const del = calls.find((c) => c.method === "DELETE");
      expect(del).toBeDefined();
      expect(del!.url).toBe("/api/compose/projects/my-proj");
    });
  });

  it("no Delete affordance for Team (board-gated on the parent)", async () => {
    current = new URLSearchParams("kind=teams&mode=edit&name=own");
    stubRoutes({
      "/api/squad/teams/own": { status: 200, body: { name: "own" } },
      "/api/squad/teams": { status: 200, body: tenantTeams },
    });
    render(<ComposeScreen />);
    // Wait for the edit form to render, then assert no delete button.
    await waitFor(() => expect(screen.getByText("Save Team (new revision)")).toBeInTheDocument());
    expect(screen.queryByTestId("compose-delete-open")).not.toBeInTheDocument();
  });
});
