// test/discussion/RoomClient.test.tsx — the Discussion route's room resolver.
// Covers R1 empty-room bootstrap (auto-open the default General thread instead
// of hanging on "Loading…"), first-thread adoption, the error state with
// retry, and the racing-open re-list fallback.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import type { Thread } from "@/lib/discussion/types";

const { client, agentsClient } = vi.hoisted(() => ({
  client: {
    listThreads: vi.fn(),
    openThread: vi.fn(),
    searchMentions: vi.fn(),
  },
  agentsClient: { getTeamOrg: vi.fn() },
}));

vi.mock("@/lib/discussion/api", () => ({
  createDiscussionClient: () => client,
}));
vi.mock("@/lib/agents/api", () => ({
  createAgentsClient: () => agentsClient,
}));
vi.mock("@/components/discussion/DiscussionRoom", () => ({
  DiscussionRoom: ({ threadId }: { threadId: string }) => (
    <div data-testid="discussion-room">{threadId}</div>
  ),
}));

import { DiscussionRoomClient } from "@/app/(app)/projects/[projectId]/discussion/RoomClient";

function thread(id: string): Thread {
  return {
    id,
    projectId: "p1",
    teamId: "t1",
    title: id,
    createdBy: "u",
    createdAt: "2026-09-26T10:00:00Z",
    messages: [],
  } as Thread;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("DiscussionRoomClient — room resolution", () => {
  it("adopts the first existing thread", async () => {
    client.listThreads.mockResolvedValue([thread("th-1"), thread("th-2")]);
    render(<DiscussionRoomClient projectId="p1" />);
    await waitFor(() =>
      expect(screen.getByTestId("discussion-room").textContent).toBe("th-1"),
    );
    expect(client.openThread).not.toHaveBeenCalled();
  });

  it("auto-opens the default General thread when the room is empty (R1 bootstrap)", async () => {
    client.listThreads.mockResolvedValue([]);
    client.openThread.mockResolvedValue(thread("th-new"));
    render(<DiscussionRoomClient projectId="p1" />);
    await waitFor(() =>
      expect(screen.getByTestId("discussion-room").textContent).toBe("th-new"),
    );
    expect(client.openThread).toHaveBeenCalledWith("p1", {
      title: "General",
      body: expect.stringContaining("discussion room"),
    });
  });

  it("re-lists and adopts when a racing open already created the thread", async () => {
    client.listThreads
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([thread("th-race")]);
    client.openThread.mockRejectedValue(new Error("conflict"));
    render(<DiscussionRoomClient projectId="p1" />);
    await waitFor(() =>
      expect(screen.getByTestId("discussion-room").textContent).toBe("th-race"),
    );
  });

  it("shows an error with retry instead of an infinite Loading… when listing fails", async () => {
    client.listThreads.mockRejectedValue(new Error("boom"));
    render(<DiscussionRoomClient projectId="p1" />);
    await waitFor(() => screen.getByTestId("room-resolve-error"));

    client.listThreads.mockResolvedValue([thread("th-back")]);
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await waitFor(() =>
      expect(screen.getByTestId("discussion-room").textContent).toBe("th-back"),
    );
  });

  it("errors when the room is empty and the auto-open cannot complete", async () => {
    client.listThreads.mockResolvedValue([]);
    client.openThread.mockRejectedValue(new Error("down"));
    render(<DiscussionRoomClient projectId="p1" />);
    await waitFor(() => screen.getByTestId("room-resolve-error"));
  });
});
