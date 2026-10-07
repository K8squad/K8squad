import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import { InboxList } from "@/components/inbox/InboxList";
import type { DiscussionClient } from "@/lib/discussion/api";

// next/navigation's useRouter needs the App Router context (absent in jsdom) — mock it so
// Enter / click navigation is observable without a Next runtime.
const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  push.mockReset();
});

type InboxItemLike = Record<string, unknown>;

/** Build a fetch mock serving a fixed inbox payload + capturing seen/unseen POST bodies. */
function stubFetch(items: InboxItemLike[], extra?: { fleet?: boolean }) {
  const seen: unknown[] = [];
  const unseen: unknown[] = [];
  const inboxCalls: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (url === "/api/inbox/seen") {
      seen.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ ok: true }) });
    }
    if (url === "/api/inbox/unseen") {
      unseen.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ ok: true }) });
    }
    inboxCalls.push(url);
    return Promise.resolve({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ fleet: extra?.fleet ?? false, items }),
    });
  });
  vi.stubGlobal("fetch", fetchMock);
  return { seen, unseen, inboxCalls, fetchMock };
}

/** A DiscussionClient stub recording answer/reject calls. */
function stubClient(): DiscussionClient & {
  answered: Array<{ projectId: string; messageId: string; body: unknown }>;
  rejected: Array<{ projectId: string; messageId: string; reason?: string }>;
} {
  const answered: Array<{ projectId: string; messageId: string; body: unknown }> = [];
  const rejected: Array<{ projectId: string; messageId: string; reason?: string }> = [];
  const client = {
    answerDecisionRequest: vi.fn((projectId: string, messageId: string, body: unknown) => {
      answered.push({ projectId, messageId, body });
      return Promise.resolve({ status: "answered" });
    }),
    rejectDecisionRequest: vi.fn((projectId: string, messageId: string, reason?: string) => {
      rejected.push({ projectId, messageId, reason });
      return Promise.resolve({ status: "rejected" });
    }),
  } as unknown as DiscussionClient;
  return Object.assign(client, { answered, rejected }) as never;
}

const approveItem = {
  key: "decision:dm-approve",
  ticketId: "wi-1",
  projectId: "squad-a/web",
  title: "Approve migration 0006",
  decisionType: "approve",
  unread: true,
  decision: { messageId: "dm-approve", allowReject: true },
};
const chooseItem = {
  key: "decision:dm-choose",
  ticketId: "wi-2",
  projectId: "squad-a/web",
  title: "Which HTTP client?",
  decisionType: "choose_one",
  unread: true,
  decision: {
    messageId: "dm-choose",
    allowReject: true,
    options: [
      { id: "reqwest", label: "reqwest", recommended: true },
      { id: "hyper", label: "hyper" },
    ],
  },
};
const freeItem = {
  key: "decision:dm-free",
  ticketId: "wi-3",
  projectId: "squad-b/api",
  title: "Which vault path?",
  decisionType: "free_form",
  unread: false,
  decision: { messageId: "dm-free", allowReject: true },
};

describe("InboxList mark-seen (F1, E1 regression)", () => {
  it("POSTs the unread keys to /api/inbox/seen once on open", async () => {
    const { seen } = stubFetch([
      { key: "proposal:m-1", projectId: "squad-a/web", title: "Needs approval", decisionType: "proposal", unread: true },
      { key: "inReview:wi-9", projectId: "squad-a/web", title: "Already read", decisionType: "review", unread: false },
    ]);
    render(<InboxList />);
    await screen.findByText("Needs approval");
    await waitFor(() => expect(seen.length).toBe(1));
    expect(seen[0]).toEqual({ keys: ["proposal:m-1"] });
  });
});

describe("InboxList inline answer (E3)", () => {
  it("approve row → Approve answers inline with an empty body", async () => {
    stubFetch([approveItem]);
    const client = stubClient();
    render(<InboxList client={client} />);
    const btn = await screen.findByTestId("inline-approve-btn");
    fireEvent.click(btn);
    await waitFor(() => expect(client.answered.length).toBe(1));
    expect(client.answered[0]).toEqual({ projectId: "squad-a/web", messageId: "dm-approve", body: {} });
    // The answered row leaves the list immediately.
    await waitFor(() => expect(screen.queryByText("Approve migration 0006")).toBeNull());
  });

  it("choose_one row → clicking an option answers with that option id", async () => {
    stubFetch([chooseItem]);
    const client = stubClient();
    render(<InboxList client={client} />);
    const opts = await screen.findAllByTestId("inline-option");
    fireEvent.click(opts[1]); // "hyper"
    await waitFor(() => expect(client.answered.length).toBe(1));
    expect(client.answered[0].body).toEqual({ selectedOptionIds: ["hyper"] });
  });

  it("approve row → Reject rejects inline", async () => {
    stubFetch([approveItem]);
    const client = stubClient();
    render(<InboxList client={client} />);
    fireEvent.click(await screen.findByTestId("inline-reject-btn"));
    await waitFor(() => expect(client.rejected.length).toBe(1));
    expect(client.rejected[0].messageId).toBe("dm-approve");
  });

  it("free_form row shows 'Open to answer' (no inline controls)", async () => {
    stubFetch([freeItem]);
    render(<InboxList />);
    await screen.findByTestId("inline-open-detail");
    expect(screen.queryByTestId("inline-approve-btn")).toBeNull();
  });
});

describe("InboxList keyboard (E3)", () => {
  it("1–9 picks the Nth option on the focused choose_one row", async () => {
    stubFetch([chooseItem]);
    const client = stubClient();
    render(<InboxList client={client} />);
    const list = await screen.findByRole("list");
    fireEvent.keyDown(list, { key: "1" });
    await waitFor(() => expect(client.answered.length).toBe(1));
    expect(client.answered[0].body).toEqual({ selectedOptionIds: ["reqwest"] });
  });

  it("A approves and Enter opens the focused row", async () => {
    stubFetch([approveItem]);
    const client = stubClient();
    render(<InboxList client={client} />);
    const list = await screen.findByRole("list");
    fireEvent.keyDown(list, { key: "Enter" });
    expect(push).toHaveBeenCalledWith("/projects/squad-a%2Fweb/issues/wi-1");
    fireEvent.keyDown(list, { key: "a" });
    await waitFor(() => expect(client.answered.length).toBe(1));
  });

  it("U marks the focused row unread via /api/inbox/unseen", async () => {
    // Row is already read so U flips it to unread (POST unseen).
    const { unseen } = stubFetch([{ ...approveItem, unread: false }]);
    render(<InboxList />);
    const list = await screen.findByRole("list");
    fireEvent.keyDown(list, { key: "u" });
    await waitFor(() => expect(unseen.length).toBe(1));
    expect(unseen[0]).toEqual({ keys: ["decision:dm-approve"] });
  });
});

describe("InboxList filters (E3)", () => {
  it("project filter narrows the list client-side", async () => {
    stubFetch([approveItem, freeItem]);
    render(<InboxList />);
    await screen.findByText("Approve migration 0006");
    expect(screen.getByText("Which vault path?")).toBeTruthy();
    fireEvent.change(screen.getByTestId("filter-project"), { target: { value: "squad-b/api" } });
    await waitFor(() => expect(screen.queryByText("Approve migration 0006")).toBeNull());
    expect(screen.getByText("Which vault path?")).toBeTruthy();
  });

  it("type filter narrows the list client-side", async () => {
    stubFetch([approveItem, chooseItem]);
    render(<InboxList />);
    await screen.findByText("Approve migration 0006");
    fireEvent.change(screen.getByTestId("filter-type"), { target: { value: "choose_one" } });
    await waitFor(() => expect(screen.queryByText("Approve migration 0006")).toBeNull());
    expect(screen.getByText("Which HTTP client?")).toBeTruthy();
  });

  it("Mine toggle appears only on fleet responses and refetches with scope=mine", async () => {
    const { inboxCalls } = stubFetch([approveItem], { fleet: true });
    render(<InboxList />);
    const mine = await screen.findByTestId("filter-mine");
    fireEvent.click(mine);
    await waitFor(() => expect(inboxCalls.some((u) => u.includes("scope=mine"))).toBe(true));
  });
});
