// test/projects/ReviewAutomationDialog.test.tsx — ISI-4764 / ISI-4750 E2: the
// "Review automation" settings dialog on the Pull Request Management header.
//
// Covers the story acceptance criteria:
//   - AC1: the four config knobs render (enable / reviewer / scope / trigger),
//     populated from the E1 GET's resolved defaults;
//   - AC2: the reviewer dropdown lists the code_review-capable team agents from
//     the D5 eligible-agents pre-filter (ISI-4779); the server's 422 stays the
//     authoritative backstop;
//   - AC3: Save PUTs exactly the write-input subset ({enabled, reviewerAgentId,
//     scope, trigger}) — never enabledBy/canEdit;
//   - AC4: a server 422 surfaces inline against the offending field, and the
//     dialog stays open (no fake success);
//   - AC5: canEdit=false renders read-only (Save disabled, inputs disabled);
//   - AC6: a 501 renders the honest "not available yet" state, no form;
//   - AC7: Cancel / Escape / scrim close without writing.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { ReviewAutomationDialog } from "@/components/github/ReviewAutomationDialog";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const PROJECT = "ns/demo";

const AGENTS = {
  agents: [
    { id: "uid-rev", name: "reviewer-bot", namespace: "ns", skillCount: 1 },
    { id: "uid-cc", name: "claude-code", namespace: "ns", skillCount: 3 },
  ],
};

const VIEW = {
  enabled: true,
  reviewerAgentId: "reviewer-bot",
  scope: "team_authored",
  trigger: "on_new_commits",
  enabledBy: "henrik",
  canEdit: true,
};

/** Route fetch by request: mount fires GET config + GET eligible-agents
 * (ISI-4779); Save fires PUT config. The eligible-agents check MUST precede the
 * config check — its URL contains the "/repo/review-automation" substring. */
function route(handlers: {
  get?: Response;
  agents?: Response;
  put?: (body: unknown) => Response;
} = {}) {
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/eligible-agents")) {
      return Promise.resolve(handlers.agents ?? jsonResponse(AGENTS, 200));
    }
    if (u.includes("/repo/review-automation")) {
      if ((init?.method ?? "GET") === "PUT") {
        const body = init?.body ? JSON.parse(init.body as string) : {};
        return Promise.resolve(handlers.put?.(body) ?? jsonResponse(VIEW, 200));
      }
      return Promise.resolve(handlers.get ?? jsonResponse(VIEW, 200));
    }
    return Promise.resolve(jsonResponse({}, 200));
  });
}

function puts(): Array<{ url: string; body: unknown }> {
  return fetchMock.mock.calls
    .filter(
      ([url, init]) =>
        String(url).includes("/repo/review-automation") &&
        (init as RequestInit | undefined)?.method === "PUT",
    )
    .map(([url, init]) => ({
      url: String(url),
      body: (init as RequestInit).body
        ? JSON.parse((init as RequestInit).body as string)
        : undefined,
    }));
}

afterEach(cleanup);
beforeEach(() => fetchMock.mockReset());

describe("ReviewAutomationDialog", () => {
  it("renders the four config knobs populated from the E1 GET (AC1)", async () => {
    route();
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={vi.fn()} />);

    await screen.findByTestId("gh-ra-form");
    expect((screen.getByTestId("ra-enable") as HTMLInputElement).checked).toBe(true);
    const reviewer = screen.getByTestId("ra-reviewer") as HTMLSelectElement;
    await waitFor(() => expect(reviewer.value).toBe("reviewer-bot"));

    // Scope + trigger reflect the served defaults.
    const scope = screen.getByTestId("ra-scope");
    const checkedScope = scope.querySelector<HTMLInputElement>("input:checked");
    expect(checkedScope?.value).toBe("team_authored");
    const trigger = screen.getByTestId("ra-trigger");
    const checkedTrigger = trigger.querySelector<HTMLInputElement>("input:checked");
    expect(checkedTrigger?.value).toBe("on_new_commits");
  });

  it("lists the eligible (code_review-capable) agents in the reviewer dropdown (AC2/ISI-4779)", async () => {
    route();
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={vi.fn()} />);
    const reviewer = (await screen.findByTestId("ra-reviewer")) as HTMLSelectElement;
    await waitFor(() => expect(reviewer.options.length).toBeGreaterThan(2));
    const names = Array.from(reviewer.options).map((o) => o.value);
    expect(names).toContain("reviewer-bot");
    expect(names).toContain("claude-code");
    // The placeholder default is present.
    expect(names[0]).toBe("");
  });

  it("PUTs exactly the write-input subset on Save (AC3)", async () => {
    const onClose = vi.fn();
    route();
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-ra-form");

    // Flip scope to "all" then save.
    const allRadio = screen
      .getByTestId("ra-scope")
      .querySelector<HTMLInputElement>('input[value="all"]')!;
    fireEvent.click(allRadio);
    fireEvent.click(screen.getByTestId("ra-save"));

    await waitFor(() => expect(puts()).toHaveLength(1));
    const { body } = puts()[0];
    expect(body).toEqual({
      enabled: true,
      reviewerAgentId: "reviewer-bot",
      scope: "all",
      trigger: "on_new_commits",
    });
    // No server-owned fields leak into the write body.
    expect(body).not.toHaveProperty("enabledBy");
    expect(body).not.toHaveProperty("canEdit");
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("surfaces a 422 inline and stays open (AC4)", async () => {
    const onClose = vi.fn();
    route({
      put: () =>
        jsonResponse(
          { error: "reviewer agent lacks the code_review capability", fields: ["reviewerAgentId"] },
          422,
        ),
    });
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-ra-form");

    fireEvent.click(screen.getByTestId("ra-save"));

    const err = await screen.findByTestId("gh-ra-save-error");
    expect(err.textContent).toMatch(/code_review/);
    // The offending field is marked invalid, the dialog is still open, no close.
    expect((screen.getByTestId("ra-reviewer") as HTMLSelectElement).getAttribute("aria-invalid")).toBe("true");
    expect(onClose).not.toHaveBeenCalled();
  });

  it("renders read-only when canEdit is false (AC5)", async () => {
    route({ get: jsonResponse({ ...VIEW, canEdit: false }, 200) });
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={vi.fn()} />);
    await screen.findByTestId("gh-ra-form");

    expect(screen.getByTestId("gh-ra-readonly")).toBeTruthy();
    expect((screen.getByTestId("ra-save") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId("ra-enable") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByTestId("ra-reviewer") as HTMLSelectElement).disabled).toBe(true);
  });

  it("renders the honest not-wired state on a 501 (AC6)", async () => {
    route({ get: jsonResponse({}, 501) });
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={vi.fn()} />);
    expect(await screen.findByTestId("gh-ra-not-wired")).toBeTruthy();
    expect(screen.queryByTestId("gh-ra-form")).toBeNull();
  });

  it("closes on Cancel without writing (AC7)", async () => {
    const onClose = vi.fn();
    route();
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-ra-form");
    fireEvent.click(screen.getByTestId("ra-cancel"));
    expect(onClose).toHaveBeenCalled();
    expect(puts()).toHaveLength(0);
  });

  it("closes on Escape (AC7)", async () => {
    const onClose = vi.fn();
    route();
    render(<ReviewAutomationDialog projectId={PROJECT} onClose={onClose} />);
    await screen.findByTestId("gh-ra-form");
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });
});
