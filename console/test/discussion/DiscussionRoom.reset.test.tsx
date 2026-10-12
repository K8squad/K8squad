// test/discussion/DiscussionRoom.reset.test.tsx — the /reset wire (ISI-5674
// WS-D): composer `/reset` → confirm dialog → WS-B endpoint → clear-signal →
// post-reset empty state. Also the SSE-driven clear and the reload persistence.

import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  waitFor,
  fireEvent,
  act,
} from "@testing-library/react";
import { DiscussionRoom } from "@/components/discussion/DiscussionRoom";
import type { DiscussionClient } from "@/lib/discussion/api";
import {
  KIND_RESET,
  type Message,
  type ResetResponse,
  type Thread,
} from "@/lib/discussion/types";
import type { RoomStreamEvent } from "@/lib/discussion/liveFeed";

function humanMsg(id: string, body: string, createdAt = "2026-10-01T10:00:00Z"): Message {
  return {
    id,
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body,
    createdAt,
  };
}

function resetMarker(createdAt: string): Message {
  return {
    id: "reset-1",
    threadId: "th-1",
    parentId: null,
    authorPrincipal: "henrik",
    authorAgentId: null,
    authorRunId: null,
    body: "— discussion cleared —",
    createdAt,
    kind: KIND_RESET,
  };
}

function makeClient(overrides: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listThreads: vi.fn(),
    getThread: vi.fn().mockResolvedValue([humanMsg("m1", "hello room")]),
    getThreadInfo: vi.fn().mockResolvedValue({ id: "th-1" } as Thread),
    openThread: vi.fn(),
    postMessage: vi.fn(),
    searchMentions: vi.fn().mockResolvedValue([]),
    getRoster: vi.fn().mockResolvedValue([]),
    retractMessage: vi.fn(),
    listProposals: vi.fn().mockResolvedValue([]),
    resetThread: vi.fn(),
    ...overrides,
  } as unknown as DiscussionClient;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("DiscussionRoom — /reset wire", () => {
  it("`/reset` opens the confirm dialog; confirming runs the endpoint and clears to the empty state", async () => {
    const marker = resetMarker("2026-10-01T11:00:00Z");
    const resetThread = vi.fn().mockResolvedValue({
      status: "reset",
      marker,
      resetAt: marker.createdAt,
      hiddenCount: 1,
    } as ResetResponse);
    const client = makeClient({ resetThread });

    render(
      <DiscussionRoom projectId="ns/proj" threadId="th-1" client={client} />,
    );
    await screen.findByText("hello room");

    // Type /reset and run it from the command menu.
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/reset" },
    });
    fireEvent.click(screen.getByTestId("slash-option"));

    // Confirm dialog appears; the endpoint is not called until confirm.
    expect(screen.getByTestId("reset-dialog")).toBeTruthy();
    expect(resetThread).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(screen.getByTestId("reset-dialog-confirm"));
    });

    expect(resetThread).toHaveBeenCalledWith("ns/proj", "th-1");
    // Transcript cleared; the marker is NOT rendered as a bubble.
    await waitFor(() => expect(screen.queryByText("hello room")).toBeNull());
    expect(screen.queryByText("— discussion cleared —")).toBeNull();
    // Post-reset empty state with the "reset by <who>" header.
    const empty = await screen.findByTestId("room-reset-empty");
    expect(empty.textContent).toContain("This discussion was reset");
    expect(screen.getByTestId("room-reset-meta").textContent).toContain(
      "reset by henrik",
    );
    // Dialog closed.
    expect(screen.queryByTestId("reset-dialog")).toBeNull();
  });

  it("cancelling the dialog leaves the transcript intact and calls nothing", async () => {
    const resetThread = vi.fn();
    const client = makeClient({ resetThread });
    render(
      <DiscussionRoom projectId="ns/proj" threadId="th-1" client={client} />,
    );
    await screen.findByText("hello room");

    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/reset" },
    });
    fireEvent.click(screen.getByTestId("slash-option"));
    fireEvent.click(screen.getByTestId("reset-dialog-cancel"));

    expect(resetThread).not.toHaveBeenCalled();
    expect(screen.getByText("hello room")).toBeTruthy();
    expect(screen.queryByTestId("room-reset-empty")).toBeNull();
  });

  it("a reset marker arriving over SSE clears the transcript (clear-signal)", async () => {
    let emit: (e: RoomStreamEvent) => void = () => {};
    const subscribe = (onEvent: (e: RoomStreamEvent) => void) => {
      emit = onEvent;
      return () => {};
    };
    const client = makeClient();
    render(
      <DiscussionRoom
        projectId="ns/proj"
        threadId="th-1"
        client={client}
        subscribe={subscribe}
      />,
    );
    await screen.findByText("hello room");

    act(() => {
      emit({
        kind: "room",
        event: {
          type: "message.created",
          message: resetMarker("2026-10-01T12:00:00Z"),
        },
      });
    });

    await waitFor(() => expect(screen.queryByText("hello room")).toBeNull());
    expect(await screen.findByTestId("room-reset-empty")).toBeTruthy();
  });

  it("a thread that was already reset shows the empty state on load (resetAt from the thread read)", async () => {
    const client = makeClient({
      getThread: vi.fn().mockResolvedValue([]),
      getThreadInfo: vi.fn().mockResolvedValue({
        id: "th-1",
        resetAt: "2026-10-01T09:00:00Z",
        resetBy: "amelia",
      } as Thread),
    });
    render(
      <DiscussionRoom projectId="ns/proj" threadId="th-1" client={client} />,
    );
    const empty = await screen.findByTestId("room-reset-empty");
    expect(empty).toBeTruthy();
    expect(screen.getByTestId("room-reset-meta").textContent).toContain(
      "reset by amelia",
    );
  });
});
