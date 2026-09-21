// test/projects/GitHubIssuesKanban.test.tsx — ISI-4673: the Issues Kanban board.
// Covers the ACs: columns reflect issue state (+ real labels), titles deep-link
// via issues/{n}, and assignee + priority are rendered from the mirror's own
// labels/assignees — never fabricated.

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  within,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import {
  GitHubIssuesKanban,
  columnFor,
  issueHref,
  priorityFor,
} from "@/components/GitHubIssuesKanban";
import type { GithubIssue } from "@/lib/github-status";

afterEach(cleanup);

const issues: GithubIssue[] = [
  {
    number: 98,
    title: "Add authentication middleware",
    state: "open",
    url: "https://gh/issues/98",
    labels: ["feature", "priority: high"],
    assignees: ["auth-agent"],
    updatedAt: new Date(Date.now() - 3_600_000).toISOString(),
  },
  {
    number: 96,
    title: "Refactor authentication service",
    state: "open",
    url: "https://gh/issues/96",
    labels: ["status: todo", "refactor"],
  },
  {
    number: 91,
    title: "Implement caching layer",
    state: "open",
    labels: ["in-progress", "cache"], // no url ⇒ fallback deep link
  },
  {
    number: 89,
    title: "Database connection timeout fix",
    state: "closed",
    url: "https://gh/issues/89",
    labels: ["bug", "blocked"],
  },
];

describe("GitHubIssuesKanban pure projection", () => {
  it("maps provider state + labels onto the four columns", () => {
    expect(columnFor(issues[0])).toBe("backlog"); // open, no status label
    expect(columnFor(issues[1])).toBe("todo"); // "status: todo"
    expect(columnFor(issues[2])).toBe("in-progress"); // "in-progress"
    expect(columnFor(issues[3])).toBe("done"); // closed wins over labels
  });

  it("derives priority from real labels only", () => {
    expect(priorityFor(issues[0])?.label).toBe("High");
    expect(priorityFor(issues[1])).toBeNull(); // no priority label ⇒ no badge
    expect(priorityFor({ number: 1, title: "x", state: "open", labels: ["p1"] })?.label).toBe("High");
    expect(priorityFor({ number: 2, title: "x", state: "open", labels: ["critical"] })?.label).toBe("Critical");
  });

  it("prefers the mirror url and falls back to issues/{n}", () => {
    expect(issueHref(issues[0])).toBe("https://gh/issues/98");
    expect(issueHref(issues[2])).toBe("https://github.com/K8squad/K8squad/issues/91");
  });
});

describe("GitHubIssuesKanban", () => {
  it("renders a column per issue state with honest counts", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const backlog = screen.getByTestId("gh-kanban-col-backlog");
    expect(within(backlog).getByText(/Add authentication middleware/)).toBeTruthy();
    expect(screen.getByTestId("gh-kanban-count-todo").textContent).toBe("1");
    expect(screen.getByTestId("gh-kanban-count-in-progress").textContent).toBe("1");
    expect(screen.getByTestId("gh-kanban-count-done").textContent).toBe("1");
  });

  it("titles each card without turning it into a navigating anchor (ISI-4758)", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const board = screen.getByTestId("gh-issues-kanban");
    const titles = within(board).getAllByTestId("gh-issue-title");
    expect(titles).toHaveLength(4);
    // The deep-link moved into the popup, so a card title is no longer an <a>.
    for (const t of titles) {
      expect(t.tagName.toLowerCase()).not.toBe("a");
      expect(t.getAttribute("href")).toBeNull();
    }
    // No fabricated "live" badge on this mirror-backed screen.
    expect(screen.queryByText(/live/i)).toBeNull();
  });

  it("shows assignee + priority, and only when the mirror carries them", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const backlog = screen.getByTestId("gh-kanban-col-backlog");
    const card = within(backlog).getByText(/Add authentication middleware/).closest(".gh-kanban-card")!;
    expect(card.querySelector('[data-testid="gh-issue-assignee"]')?.textContent).toBe("auth-agent");
    expect(card.querySelector('[data-testid="gh-issue-priority"]')?.textContent).toBe("High");
    // Unassigned card renders the honest absence, not an invented agent.
    const todo = screen.getByTestId("gh-kanban-col-todo");
    const todoCard = within(todo).getByText(/Refactor authentication service/).closest(".gh-kanban-card")!;
    expect(todoCard.querySelector('[data-testid="gh-issue-assignee"]')?.textContent).toBe("Unassigned");
    expect(todoCard.querySelector('[data-testid="gh-issue-priority"]')).toBeNull();
  });

  it("surfaces blocked + prioritised in stats and the priority queue", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    expect(screen.getByTestId("gh-issue-blocked").textContent).toBe("Blocked");
    const stats = screen.getByTestId("gh-issues-stats").textContent ?? "";
    expect(stats).toContain("Open");
    expect(stats).toContain("Blocked");
    const queue = screen.getByTestId("gh-priority-queue");
    expect(within(queue).getByText(/Add authentication middleware/)).toBeTruthy();
    expect(within(queue).queryByText(/Refactor authentication service/)).toBeNull(); // unprioritised
  });

  it("renders nothing for an empty issue set (parent owns the empty state)", () => {
    const { container } = render(<GitHubIssuesKanban issues={[]} />);
    expect(container.firstChild).toBeNull();
  });
});

// ISI-4758 (ISI-4749 epic 1): card → popup UX shell.
describe("GitHubIssuesKanban card popup", () => {
  const firstCard = () => {
    const board = screen.getByTestId("gh-issues-kanban");
    return within(board)
      .getAllByTestId("gh-issue-card")
      .find((c) => c.textContent?.includes("#98"))!;
  };

  it("opens the popup on card click with the issue identity as its label", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    expect(screen.queryByTestId("gh-issue-popup")).toBeNull();
    fireEvent.click(firstCard());
    const popup = screen.getByTestId("gh-issue-popup");
    expect(popup.getAttribute("role")).toBe("dialog");
    expect(popup.getAttribute("aria-modal")).toBe("true");
    expect(popup.textContent).toContain("#98");
    expect(popup.textContent).toContain("Add authentication middleware");
  });

  it("is keyboard-openable (Enter / Space) on the focusable card", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const card = firstCard();
    expect(card.getAttribute("tabindex")).toBe("0");
    expect(card.getAttribute("role")).toBe("button");
    fireEvent.keyDown(card, { key: "Enter" });
    expect(screen.getByTestId("gh-issue-popup")).toBeTruthy();
  });

  it("hosts the working 'Open on GitHub' new-tab deep-link inside the popup", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    fireEvent.click(firstCard());
    const link = screen.getByTestId("gh-issue-popup-open-github");
    expect(link.tagName.toLowerCase()).toBe("a");
    expect(link.getAttribute("href")).toBe("https://gh/issues/98");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
  });

  it("falls the deep-link back to issues/{n} for a sparse mirror row", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const board = screen.getByTestId("gh-issues-kanban");
    const sparse = within(board)
      .getAllByTestId("gh-issue-card")
      .find((c) => c.textContent?.includes("#91"))!;
    fireEvent.click(sparse);
    expect(
      screen.getByTestId("gh-issue-popup-open-github").getAttribute("href"),
    ).toBe("https://github.com/K8squad/K8squad/issues/91");
  });

  it("reserves an empty assign slot for epic 3 (no fabricated control)", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    fireEvent.click(firstCard());
    const slot = screen.getByTestId("gh-issue-assign-slot");
    expect(slot).toBeTruthy();
    expect(slot.childElementCount).toBe(0);
    expect(slot.textContent).toBe("");
  });

  it("closes on Escape and on backdrop click", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    fireEvent.click(firstCard());
    expect(screen.getByTestId("gh-issue-popup")).toBeTruthy();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByTestId("gh-issue-popup")).toBeNull();

    // Reopen and close via the backdrop scrim.
    fireEvent.click(firstCard());
    fireEvent.click(screen.getByTestId("gh-issue-popup-scrim"));
    expect(screen.queryByTestId("gh-issue-popup")).toBeNull();
  });

  it("closes via the × button but not when clicking inside the popup", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    fireEvent.click(firstCard());
    // A click on the popup body must not fall through to the scrim's close.
    fireEvent.click(screen.getByTestId("gh-issue-popup"));
    expect(screen.getByTestId("gh-issue-popup")).toBeTruthy();
    fireEvent.click(screen.getByTestId("gh-issue-popup-close"));
    expect(screen.queryByTestId("gh-issue-popup")).toBeNull();
  });
});

// ISI-4759 (ISI-4749 epic 3): wire the popup to assign-&-dispatch + agent dropdown.
// The control fills the Epic-1 slot ONLY with a project context, loads the roster
// from GET /api/squad/agents, and dispatches via the Epic-2 bridge
// POST /api/projects/{id}/github/issues/{n}/assign. fetch is stubbed so the real
// assignAndDispatch / listSquadAgents code paths (URL, body, status→message) run.
describe("GitHubIssuesKanban assign & dispatch (epic 3)", () => {
  const fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  beforeEach(() => fetchMock.mockReset());

  const PROJECT = "ns/demo";
  const ROSTER = {
    agents: [
      { id: "uid-alice", name: "alice" },
      { id: "uid-bob", name: "bob" },
    ],
  };

  function jsonResponse(body: unknown, status = 200): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
  }

  /** mount fires GET /api/squad/agents; the submit fires POST .../assign. */
  function route(handlers: { agents?: Response; assign?: (body: any) => Response }) {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      const u = String(url);
      if (u.includes("/api/squad/agents")) {
        return Promise.resolve(handlers.agents ?? jsonResponse(ROSTER, 200));
      }
      if (u.includes("/github/issues/") && u.includes("/assign")) {
        const body = init?.body ? JSON.parse(init.body as string) : {};
        return Promise.resolve(
          handlers.assign?.(body) ??
            jsonResponse(
              {
                workItemId: "wi-1",
                issueRef: "K8squad/K8squad#98",
                created: true,
                dispatch: { requestedAgent: body.agentId, fromState: "backlog", toState: "todo" },
              },
              200,
            ),
        );
      }
      return Promise.resolve(jsonResponse({}, 200));
    });
  }

  const assignPosts = () =>
    fetchMock.mock.calls.filter(
      ([url, init]) =>
        String(url).includes("/assign") &&
        (init as RequestInit | undefined)?.method === "POST",
    );

  const openPopup = () => {
    const board = screen.getByTestId("gh-issues-kanban");
    const card = within(board)
      .getAllByTestId("gh-issue-card")
      .find((c) => c.textContent?.includes("#98"))!;
    fireEvent.click(card);
  };

  it("renders no assign control without a project context (honest, non-functional slot stays empty)", () => {
    route({});
    render(<GitHubIssuesKanban issues={issues} />);
    openPopup();
    expect(screen.queryByTestId("gh-issue-assign")).toBeNull();
    expect(screen.getByTestId("gh-issue-assign-slot").childElementCount).toBe(0);
  });

  it("AC1: populates the dropdown from the roster (value = agent name)", async () => {
    route({});
    render(<GitHubIssuesKanban issues={issues} projectId={PROJECT} />);
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() =>
      expect(within(select).getByText("alice")).toBeTruthy(),
    );
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toContain("alice");
    expect(values).toContain("bob");
    // Submit is disabled until an agent is chosen.
    expect((screen.getByTestId("gh-issue-assign-submit") as HTMLButtonElement).disabled).toBe(true);
  });

  it("AC1: an empty roster shows 'No agents available' and keeps submit disabled", async () => {
    route({ agents: jsonResponse({ agents: [] }, 200) });
    render(<GitHubIssuesKanban issues={issues} projectId={PROJECT} />);
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() => expect(select.disabled).toBe(true));
    expect(select.textContent).toContain("No agents available");
    expect((screen.getByTestId("gh-issue-assign-submit") as HTMLButtonElement).disabled).toBe(true);
  });

  it("AC2/AC3: selecting an agent + confirm calls the bridge, hands the result up, and closes", async () => {
    const onAssigned = vi.fn();
    route({});
    render(
      <GitHubIssuesKanban issues={issues} projectId={PROJECT} onAssigned={onAssigned} />,
    );
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getByText("alice")).toBeTruthy());
    fireEvent.change(select, { target: { value: "alice" } });
    fireEvent.click(screen.getByTestId("gh-issue-assign-submit"));

    await waitFor(() => expect(onAssigned).toHaveBeenCalledTimes(1));
    expect(onAssigned).toHaveBeenCalledWith(
      expect.objectContaining({ workItemId: "wi-1", created: true, issueRef: "K8squad/K8squad#98" }),
    );
    // Popup closed on success.
    await waitFor(() => expect(screen.queryByTestId("gh-issue-popup")).toBeNull());
    // Bridge was hit with the agent NAME + full issue url, at the project-scoped path.
    const posts = assignPosts();
    expect(posts).toHaveLength(1);
    const [url, init] = posts[0];
    expect(String(url)).toContain("/github/issues/98/assign");
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      agentId: "alice",
      url: "https://gh/issues/98",
    });
  });

  it("AC4: a 403 surfaces the mapped inline error, keeps the popup open, preserves selection", async () => {
    route({ assign: () => jsonResponse({ error: "nope" }, 403) });
    render(<GitHubIssuesKanban issues={issues} projectId={PROJECT} />);
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getByText("bob")).toBeTruthy());
    fireEvent.change(select, { target: { value: "bob" } });
    fireEvent.click(screen.getByTestId("gh-issue-assign-submit"));

    const err = await screen.findByTestId("gh-issue-assign-error");
    expect(err.textContent).toBe("You can't dispatch this agent to this issue.");
    // Popup stays open, selection preserved so the operator can retry.
    expect(screen.getByTestId("gh-issue-popup")).toBeTruthy();
    expect((screen.getByTestId("gh-issue-assign-select") as HTMLSelectElement).value).toBe("bob");
  });

  it("§6: a 409 is surfaced as a soft, informational 'already assigned' state", async () => {
    route({ assign: () => jsonResponse({ error: "dup" }, 409) });
    render(<GitHubIssuesKanban issues={issues} projectId={PROJECT} />);
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getByText("alice")).toBeTruthy());
    fireEvent.change(select, { target: { value: "alice" } });
    fireEvent.click(screen.getByTestId("gh-issue-assign-submit"));

    const err = await screen.findByTestId("gh-issue-assign-error");
    expect(err.textContent).toBe("This issue is already assigned to an agent.");
    expect(screen.getByTestId("gh-issue-popup")).toBeTruthy();
  });

  it("AC5 honesty: a successful assign never writes GitHub assignees", async () => {
    route({});
    render(<GitHubIssuesKanban issues={issues} projectId={PROJECT} />);
    openPopup();
    const select = (await screen.findByTestId("gh-issue-assign-select")) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getByText("alice")).toBeTruthy());
    fireEvent.change(select, { target: { value: "alice" } });
    fireEvent.click(screen.getByTestId("gh-issue-assign-submit"));
    await waitFor(() => expect(assignPosts()).toHaveLength(1));

    // The only mutating call is the dispatch bridge; its body carries no assignees,
    // and nothing writes to a GitHub assignees endpoint (ADR-0013, v1).
    const mutating = fetchMock.mock.calls.filter(
      ([, init]) => (init as RequestInit | undefined)?.method === "POST",
    );
    expect(mutating).toHaveLength(1);
    const body = JSON.parse((mutating[0][1] as RequestInit).body as string);
    expect(body).not.toHaveProperty("assignees");
    expect(fetchMock.mock.calls.some(([u]) => String(u).includes("assignees"))).toBe(false);
  });
});
