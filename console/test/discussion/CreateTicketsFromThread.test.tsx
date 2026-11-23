// test/discussion/CreateTicketsFromThread.test.tsx — the thread→tickets sheet
// (ISI-7710, UX spec ISI-7709 §2-§5): pre-seed, single-draft wire body with
// provenance ids, success view with links, error-keeps-draft, dup heads-up,
// and the not-hosted degrade on capability-less clients.

import { describe, it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { CreateTicketsFromThread } from "@/components/discussion/CreateTicketsFromThread";
import {
  DiscussionApiError,
  type DiscussionClient,
} from "@/lib/discussion/api";
import type {
  CreatedThreadTicket,
  ThreadTicketsResponse,
} from "@/lib/discussion/types";
import type { ThreadDigest } from "@/lib/discussion/threadDigest";

afterEach(cleanup);

const seed: ThreadDigest = {
  title: "Fix the login flake",
  body: "Thread summary:\n- **henrik**: login flakes on reload",
  sourceMessageIds: ["m1", "m2"],
};

function ticket(id: string, title: string): CreatedThreadTicket {
  return {
    id,
    title,
    state: "backlog",
    priority: null,
    url: "/projects/p/issues/" + id,
    labels: ["ksquad.discussion.thread=th-1"],
  };
}

function ok(created: CreatedThreadTicket[]): ThreadTicketsResponse {
  return {
    status: "created",
    threadId: "th-1",
    count: created.length,
    created,
  };
}

describe("CreateTicketsFromThread (ISI-7710)", () => {
  it("pre-seeds the title and the digest body from the seed", () => {
    render(
      <CreateTicketsFromThread
        projectId="p1"
        threadId="th-1"
        client={{ createThreadTickets: vi.fn() } as unknown as DiscussionClient}
        seed={seed}
        existingTitles={[]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    expect(
      screen.getByTestId("thread-tickets-title").getAttribute("value"),
    ).toBe("Fix the login flake");
    expect(
      screen.getByTestId("thread-tickets-description").textContent,
    ).toContain("login flakes on reload");
  });

  it("submits ONE draft carrying the provenance ids and hands back the created item", async () => {
    const fn = vi
      .fn()
      .mockResolvedValue(ok([ticket("w1", "Fix the login flake")]));
    const onCreated = vi.fn();
    render(
      <CreateTicketsFromThread
        projectId="p1"
        threadId="th-1"
        client={{ createThreadTickets: fn } as unknown as DiscussionClient}
        seed={seed}
        existingTitles={[]}
        onCreated={onCreated}
        onClose={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByTestId("thread-tickets-submit"));
    await waitFor(() =>
      expect(screen.getByTestId("thread-tickets-created")).toBeTruthy(),
    );
    expect(fn).toHaveBeenCalledWith("p1", "th-1", [
      {
        title: "Fix the login flake",
        body: seed.body,
        sourceMessageIds: ["m1", "m2"],
      },
    ]);
    expect(onCreated).toHaveBeenCalledWith([
      ticket("w1", "Fix the login flake"),
    ]);
    const link = screen.getByTestId("thread-tickets-created-link");
    expect(link.getAttribute("href")).toBe("/projects/p/issues/w1");
    expect(link.textContent).toBe("Fix the login flake");
  });

  it("a 403 keeps the sheet open with the draft intact", async () => {
    const fn = vi
      .fn()
      .mockRejectedValue(new DiscussionApiError(403, "not-found"));
    render(
      <CreateTicketsFromThread
        projectId="p1"
        threadId="th-1"
        client={{ createThreadTickets: fn } as unknown as DiscussionClient}
        seed={seed}
        existingTitles={[]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByTestId("thread-tickets-submit"));
    await waitFor(() =>
      expect(screen.getByTestId("thread-tickets-error").textContent).toContain(
        "permission",
      ),
    );
    // Draft intact: the seeded title is still editable in the open form.
    expect(
      screen.getByTestId("thread-tickets-title").getAttribute("value"),
    ).toBe("Fix the login flake");
    expect(
      screen.getByTestId("thread-tickets-submit").hasAttribute("disabled"),
    ).toBe(false);
  });

  it("surfaces the non-blocking duplicate heads-up (UX §5)", () => {
    render(
      <CreateTicketsFromThread
        projectId="p1"
        threadId="th-1"
        client={{ createThreadTickets: vi.fn() } as unknown as DiscussionClient}
        seed={seed}
        existingTitles={["Earlier ticket", "Second earlier"]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const warn = screen.getByTestId("thread-tickets-dup-warn");
    expect(warn.textContent).toContain("Earlier ticket");
    expect(warn.getAttribute("role")).not.toBe("alert"); // non-blocking
  });

  it("degrades with the not-hosted copy on a capability-less client", async () => {
    render(
      <CreateTicketsFromThread
        projectId="p1"
        threadId="th-1"
        client={{} as unknown as DiscussionClient}
        seed={seed}
        existingTitles={[]}
        onCreated={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByTestId("thread-tickets-submit"));
    await waitFor(() =>
      expect(screen.getByTestId("thread-tickets-error").textContent).toContain(
        "isn't hosted",
      ),
    );
  });
});
