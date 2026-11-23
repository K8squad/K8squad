// test/discussion/DiscussionRoom.tickets.test.tsx — the ISI-7710 room wiring:
// fail-closed viewer gate on the header-strip entry, disabled on an empty
// thread, sheet open + pre-seed, and the create flow ending in the linked
// post-back message (references) + the room Linked-tickets affordance.

import { describe, it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { DiscussionClient } from "@/lib/discussion/api";
import { fetchViewerRole } from "@/lib/tickets/api";
import type { Message, Thread } from "@/lib/discussion/types";

vi.mock("@/lib/tickets/api", () => ({
  fetchViewerRole: vi.fn().mockResolvedValue("user"),
}));

afterEach(() => {
  cleanup();
  vi.mocked(fetchViewerRole).mockReset().mockResolvedValue("user");
});

function humanMsg(id: string, body: string): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt: "2026-10-01T10:00:00Z",
  };
}

const threadInfo: Thread = {
  id: "th-1",
  projectId: "p1",
  teamId: "t1",
  title: "Fix the login flake",
  createdBy: "henrik",
  createdAt: "2026-10-01T09:00:00Z",
};

function makeClient(overrides: Record<string, unknown> = {}): DiscussionClient {
  return {
    getThread: vi
      .fn()
      .mockResolvedValue([humanMsg("m1", "hello room"), humanMsg("m2", "login flakes on reload")]),
    getThreadInfo: vi.fn().mockResolvedValue(threadInfo),
    listProposals: vi.fn().mockResolvedValue([]),
    getActivePartySession: vi.fn().mockResolvedValue(null),
    postMessage: vi.fn(),
    createThreadTickets: vi.fn(),
    ...overrides,
  } as unknown as DiscussionClient;
}

function renderedMessages(client: DiscussionClient) {
  return client.getThread as unknown as ReturnType<typeof vi.fn>;
}

describe("DiscussionRoom — create tickets from thread (ISI-7710)", () => {
  it("is fail-closed while the role is unresolved (no entry rendered)", async () => {
    vi.mocked(fetchViewerRole).mockReturnValue(
      new Promise(() => {}) as Promise<string>,
    );
    render(
      <DiscussionRoom projectId="p1" threadId="th-1" client={makeClient()} />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("threads")).toBeTruthy(),
    );
    expect(screen.queryByTestId("room-tickets-bar")).toBeNull();
  });

  it("hides the entry from a viewer entirely", async () => {
    vi.mocked(fetchViewerRole).mockResolvedValue("viewer");
    render(
      <DiscussionRoom projectId="p1" threadId="th-1" client={makeClient()} />,
    );
    await waitFor(() => expect(screen.getByTestId("threads")).toBeTruthy());
    expect(screen.queryByTestId("room-tickets-bar")).toBeNull();
  });

  it("disables the entry on an empty (or reset) thread", async () => {
    const client = makeClient({ getThread: vi.fn().mockResolvedValue([]) });
    render(<DiscussionRoom projectId="p1" threadId="th-1" client={client} />);
    await waitFor(() =>
      expect(screen.getByTestId("room-create-tickets")).toBeTruthy(),
    );
    expect(
      screen.getByTestId("room-create-tickets").hasAttribute("disabled"),
    ).toBe(true);
    expect(renderedMessages(client)).toBeTruthy();
  });

  it("opens the sheet pre-seeded from the thread title + digest", async () => {
    render(
      <DiscussionRoom projectId="p1" threadId="th-1" client={makeClient()} />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("room-create-tickets")).toBeTruthy(),
    );
    fireEvent.click(screen.getByTestId("room-create-tickets"));
    expect(screen.getByTestId("thread-tickets-sheet")).toBeTruthy();
    expect(
      screen.getByTestId("thread-tickets-title").getAttribute("value"),
    ).toBe("Fix the login flake");
    expect(
      screen.getByTestId("thread-tickets-description").textContent,
    ).toContain("login flakes on reload");
  });

  it("creates the ticket, posts the linked post-back with references, and shows the affordance", async () => {
    const created = {
      id: "w1",
      title: "Fix the login flake",
      state: "backlog",
      priority: null,
      url: "/projects/p/issues/w1",
      labels: ["ksquad.discussion.thread=th-1"],
    };
    const create = vi
      .fn()
      .mockResolvedValue({ status: "created", threadId: "th-1", count: 1, created: [created] });
    const postMessage = vi.fn().mockResolvedValue(humanMsg("pb1", "post-back"));
    const client = makeClient({
      createThreadTickets: create,
      postMessage,
    });
    render(<DiscussionRoom projectId="p1" threadId="th-1" client={client} />);
    await waitFor(() =>
      expect(screen.getByTestId("room-create-tickets")).toBeTruthy(),
    );
    fireEvent.click(screen.getByTestId("room-create-tickets"));
    fireEvent.click(screen.getByTestId("thread-tickets-submit"));
    // Success view with the created link (UX §4).
    await waitFor(() =>
      expect(screen.getByTestId("thread-tickets-created-link")).toBeTruthy(),
    );
    expect(create).toHaveBeenCalledWith("p1", "th-1", [
      expect.objectContaining({ title: "Fix the login flake" }),
    ]);
    // The post-back rides the TicketReference plumbing (compose.ts).
    await waitFor(() => expect(postMessage).toHaveBeenCalled());
    const arg = postMessage.mock.calls[0][2] as {
      references?: { workItemId: string; title?: string }[];
    };
    expect(arg.references).toEqual([
      { workItemId: "w1", title: "Fix the login flake" },
    ]);
    // Room-level Linked tickets affordance (durable record of the flow).
    await waitFor(() =>
      expect(screen.getByTestId("room-linked-tickets")).toBeTruthy(),
    );
    expect(
      screen.getByTestId("room-linked-tickets").textContent,
    ).toContain("Fix the login flake");
  });
});
