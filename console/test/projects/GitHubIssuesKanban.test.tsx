// test/projects/GitHubIssuesKanban.test.tsx — ISI-4673: the Issues Kanban board.
// Covers the ACs: columns reflect issue state (+ real labels), titles deep-link
// via issues/{n}, and assignee + priority are rendered from the mirror's own
// labels/assignees — never fabricated.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
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

  it("deep-links every issue title (url or issues/{n})", () => {
    render(<GitHubIssuesKanban issues={issues} />);
    const board = screen.getByTestId("gh-issues-kanban");
    const links = within(board).getAllByTestId("gh-issue-title");
    expect(links).toHaveLength(4);
    const first = links.find((l) => l.textContent?.includes("#98"));
    expect(first?.getAttribute("href")).toBe("https://gh/issues/98");
    const fallback = links.find((l) => l.textContent?.includes("#91"));
    expect(fallback?.getAttribute("href")).toBe("https://github.com/K8squad/K8squad/issues/91");
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
