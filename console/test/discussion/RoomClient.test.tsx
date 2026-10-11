// Routing + new/open-chat coverage for the Discussion room client wrapper
// (ISI-5673 / ISI-5598 WS-C): the active thread is driven by `?thread={id}`
// (replacing the old hardcoded threads[0] pick); open-existing pushes the query;
// new-chat POSTs a thread and selects it; the empty project bootstraps a thread.
//
// DiscussionRoom is mocked to a marker so these assertions stay on the wrapper's
// thread-resolution + URL wiring, not the room's heavy load path.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import type { DiscussionClient } from "@/lib/discussion/api";
import type { Thread } from "@/lib/discussion/types";

// --- next/navigation: a controllable URL so router.push/replace are observable
//     and the next render reads the updated ?thread param. -------------------
const push = vi.fn((url: string) => {
  params = new URLSearchParams(url.split("?")[1] ?? "");
});
const replace = vi.fn((url: string) => {
  params = new URLSearchParams(url.split("?")[1] ?? "");
});
let params = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace }),
  usePathname: () => "/projects/p1/discussion",
  useSearchParams: () => params,
}));

// DiscussionRoom → a marker echoing the threadId it was mounted with.
vi.mock("@/components/discussion/DiscussionRoom", () => ({
  DiscussionRoom: ({ threadId }: { threadId: string }) => (
    <div data-testid="mock-room">room:{threadId}</div>
  ),
}));

import { DiscussionRoomClient } from "@/app/(app)/projects/[projectId]/discussion/RoomClient";

afterEach(cleanup);
beforeEach(() => {
  push.mockClear();
  replace.mockClear();
  params = new URLSearchParams();
});

function thread(id: string, title: string, createdAt: string): Thread {
  return {
    id,
    projectId: "p1",
    teamId: "t1",
    title,
    createdBy: "alice",
    createdAt,
  };
}

const THREADS = [
  thread("newest", "Planning", "2026-10-12T11:30:00Z"),
  thread("older", "Retro", "2026-10-10T12:00:00Z"),
];

function stubClient(over: Partial<DiscussionClient> = {}): DiscussionClient {
  return {
    listThreads: vi.fn(async () => THREADS),
    openThread: vi.fn(async (_p, input) =>
      thread("fresh", input.title, "2026-10-12T12:00:00Z"),
    ),
    getRoster: vi.fn(async () => []),
    searchMentions: vi.fn(async () => []),
    ...over,
  } as unknown as DiscussionClient;
}

const noOverview = () => Promise.resolve({ ok: false } as Response);

function renderClient(client: DiscussionClient) {
  return render(
    <DiscussionRoomClient
      projectId="p1"
      client={client}
      loadOverview={noOverview}
    />,
  );
}

describe("<DiscussionRoomClient> — ISI-5673 routing", () => {
  it("with no ?thread, selects the newest thread and canonicalizes the URL", async () => {
    renderClient(stubClient());
    await waitFor(() =>
      expect(screen.getByTestId("mock-room").textContent).toBe("room:newest"),
    );
    // The URL is pinned to the resolved thread (replace — no history entry).
    expect(replace).toHaveBeenCalledWith(
      "/projects/p1/discussion?thread=newest",
      { scroll: false },
    );
  });

  it("honours a ?thread param pointing at a non-first thread", async () => {
    params = new URLSearchParams("thread=older");
    renderClient(stubClient());
    await waitFor(() =>
      expect(screen.getByTestId("mock-room").textContent).toBe("room:older"),
    );
    // Already canonical → no replace churn.
    expect(replace).not.toHaveBeenCalled();
  });

  it("open-existing: clicking a row pushes ?thread and loads that thread", async () => {
    const { rerender } = renderClient(stubClient());
    await waitFor(() => screen.getByTestId("discussions-row-older"));
    fireEvent.click(screen.getByTestId("discussions-row-older"));
    expect(push).toHaveBeenCalledWith(
      "/projects/p1/discussion?thread=older",
      { scroll: false },
    );
    // The push updated the URL; the next render shows the selected thread.
    rerender(
      <DiscussionRoomClient
        projectId="p1"
        client={stubClient()}
        loadOverview={noOverview}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("mock-room").textContent).toBe("room:older"),
    );
  });

  it("new-chat: commits a POST with the typed title and selects the new thread", async () => {
    const client = stubClient();
    renderClient(client);
    await waitFor(() => screen.getByTestId("discussions-new-chat"));
    fireEvent.click(screen.getByTestId("discussions-new-chat"));
    fireEvent.change(screen.getByTestId("discussions-new-chat-input"), {
      target: { value: "Release prep" },
    });
    fireEvent.keyDown(screen.getByTestId("discussions-new-chat-input"), {
      key: "Enter",
    });
    await waitFor(() =>
      expect(client.openThread).toHaveBeenCalledWith("p1", {
        title: "Release prep",
        body: expect.any(String),
      }),
    );
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith(
        "/projects/p1/discussion?thread=fresh",
        { scroll: false },
      ),
    );
  });

  it("empty project: bootstraps the default thread via openThread", async () => {
    const client = stubClient({
      listThreads: vi.fn(async () => [] as Thread[]),
      openThread: vi.fn(async () =>
        thread("general", "General", "2026-10-12T12:00:00Z"),
      ),
    });
    renderClient(client);
    await waitFor(() =>
      expect(screen.getByTestId("mock-room").textContent).toBe("room:general"),
    );
    expect(client.openThread).toHaveBeenCalledWith(
      "p1",
      expect.objectContaining({ title: "General" }),
    );
  });

  it("surfaces a list-read failure as the panel error state", async () => {
    const client = stubClient({
      listThreads: vi.fn(async () => {
        throw new Error("boom");
      }),
    });
    renderClient(client);
    await waitFor(() =>
      expect(screen.getByTestId("discussions-panel-error")).toBeTruthy(),
    );
    expect(screen.getByTestId("room-resolve-error")).toBeTruthy();
  });

  it("re-lists and adopts when a racing open already created the thread", async () => {
    const listThreads = vi
      .fn<DiscussionClient["listThreads"]>()
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([thread("race", "Race", "2026-10-12T12:00:00Z")]);
    const client = stubClient({
      listThreads,
      openThread: vi.fn(async () => {
        throw new Error("conflict");
      }),
    });
    renderClient(client);
    await waitFor(() =>
      expect(screen.getByTestId("mock-room").textContent).toBe("room:race"),
    );
  });

  it("errors when the room is empty and the auto-open cannot complete", async () => {
    const client = stubClient({
      listThreads: vi.fn(async () => [] as Thread[]),
      openThread: vi.fn(async () => {
        throw new Error("down");
      }),
    });
    renderClient(client);
    await waitFor(() =>
      expect(screen.getByTestId("room-resolve-error")).toBeTruthy(),
    );
  });
});
