import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { Composer } from "@/components/discussion/Composer";
import type { MentionSuggestion } from "@/lib/discussion/types";

afterEach(cleanup);

// AC3 at the component boundary: the composer hands out ONLY { body, parentId? }.
// AC5: it exposes no coordination control.

describe("<Composer> — AC3 server-stamp boundary", () => {
  it("posting a new message emits { body } with no author field", () => {
    const onPost = vi.fn();
    render(<Composer onPost={onPost} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "hello room" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onPost).toHaveBeenCalledTimes(1);
    const arg = onPost.mock.calls[0][0];
    expect(Object.keys(arg).sort()).toEqual(["body"]);
    expect(arg).not.toHaveProperty("author");
  });

  it("replying emits { body, parentId } only", () => {
    const onPost = vi.fn();
    render(<Composer parentId="p-7" onPost={onPost} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "re: hi" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    const arg = onPost.mock.calls[0][0];
    expect(arg).toEqual({ body: "re: hi", parentId: "p-7" });
  });

  it("submit is disabled for an empty body (no phantom posts)", () => {
    render(<Composer onPost={vi.fn()} />);
    expect(screen.getByTestId("composer-submit")).toBeDisabled();
  });

  it("renders no coordination control (only a post/reply affordance)", () => {
    const { container } = render(<Composer onPost={vi.fn()} />);
    const buttons = Array.from(container.querySelectorAll("button"));
    expect(buttons).toHaveLength(1);
    expect(buttons[0].textContent).toMatch(/^(Post|Reply)$/);
  });
});

describe("<Composer> — audience selector (ISI-4929, plan §4.2)", () => {
  const agents = [
    { id: "agent-7", name: "Amelia" },
    { id: "agent-8", name: "Winston" },
  ];

  it("no directTargets ⇒ no audience selector; posts stay party-minimal", () => {
    const onPost = vi.fn();
    render(<Composer onPost={onPost} />);
    expect(screen.queryByTestId("audience-select")).toBeNull();
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "hi" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(Object.keys(onPost.mock.calls[0][0]).sort()).toEqual(["body"]);
  });

  it("directTargets render a selector defaulting to party", () => {
    render(<Composer onPost={vi.fn()} directTargets={agents} />);
    const select = screen.getByTestId("audience-select") as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(select.querySelectorAll("option")).toHaveLength(3); // party + 2 agents
  });

  it("picking a direct target emits audience: direct:{agentId}", () => {
    const onPost = vi.fn();
    render(<Composer onPost={onPost} directTargets={agents} />);
    fireEvent.change(screen.getByTestId("audience-select"), {
      target: { value: "agent-7" },
    });
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "psst" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onPost.mock.calls[0][0]).toEqual({
      body: "psst",
      audience: "direct:agent-7",
    });
  });

  it("switching back to party omits the audience key", () => {
    const onPost = vi.fn();
    render(<Composer onPost={onPost} directTargets={agents} />);
    fireEvent.change(screen.getByTestId("audience-select"), {
      target: { value: "agent-8" },
    });
    fireEvent.change(screen.getByTestId("audience-select"), {
      target: { value: "" },
    });
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "back to all" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(Object.keys(onPost.mock.calls[0][0]).sort()).toEqual(["body"]);
  });
});

const MENTIONS: MentionSuggestion[] = [
  { type: "agent", id: "Amelia", displayName: "Amelia", state: "idle" },
  {
    type: "work_item",
    id: "wi-1",
    displayName: "Fix the flaky test",
    state: "todo",
    rank: 1,
  },
];

describe("<Composer> — @-mention popover (ISI-4929, plan §4.3)", () => {
  it("typing @fragment pops suggestions from the search", async () => {
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={vi.fn()} searchMentions={searchMentions} />);
    const body = screen.getByTestId("composer-body");
    fireEvent.change(body, { target: { value: "ping @am" } });
    await waitFor(() => {
      expect(screen.getByTestId("mention-popover")).toBeTruthy();
    });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option")).toHaveLength(2);
    });
    expect(searchMentions).toHaveBeenCalledWith("am");
    // agents lead the popover
    expect(screen.getAllByTestId("mention-option")[0]).toHaveAttribute(
      "data-mention-type",
      "agent",
    );
  });

  it("picking a suggestion replaces the fragment with the @Name token", async () => {
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={vi.fn()} searchMentions={searchMentions} />);
    const body = screen.getByTestId("composer-body") as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: "ping @am" } });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option").length).toBe(2);
    });
    fireEvent.click(screen.getAllByTestId("mention-option")[0]);
    expect(body.value).toBe("ping @Amelia ");
    // the popover closes after insert
    expect(screen.queryByTestId("mention-popover")).toBeNull();
  });

  it("no @ trigger (or no search wired) ⇒ no popover", () => {
    render(<Composer onPost={vi.fn()} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "just words" },
    });
    expect(screen.queryByTestId("mention-popover")).toBeNull();
  });

  it("a search failure degrades to an empty popover, never a throw", async () => {
    const searchMentions = vi.fn().mockRejectedValue(new Error("boom"));
    render(<Composer onPost={vi.fn()} searchMentions={searchMentions} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "ping @zz" },
    });
    await waitFor(() => {
      expect(screen.getByTestId("mention-popover")).toBeTruthy();
    });
    await waitFor(() => {
      expect(screen.getByTestId("mention-empty")).toBeTruthy();
    });
  });
});

describe("<Composer> — `#` ticket picker (ISI-5167 / ISI-5134 S3)", () => {
  it("typing #fragment opens a picker scoped to work_item suggestions", async () => {
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={vi.fn()} searchMentions={searchMentions} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "see #fl" },
    });
    await waitFor(() => {
      expect(screen.getByTestId("mention-popover")).toBeTruthy();
    });
    // The `#` trigger filters out the agent row — only the ticket remains.
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option")).toHaveLength(1);
    });
    expect(searchMentions).toHaveBeenCalledWith("fl");
    expect(screen.getAllByTestId("mention-option")[0]).toHaveAttribute(
      "data-mention-type",
      "work_item",
    );
  });

  it("selecting a ticket inserts a #Title token and adds a structured ref to the payload", async () => {
    const onPost = vi.fn();
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={onPost} searchMentions={searchMentions} />);
    const body = screen.getByTestId("composer-body") as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: "see #fl" } });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option").length).toBe(1);
    });
    fireEvent.click(screen.getAllByTestId("mention-option")[0]);
    expect(body.value).toBe("see #Fix the flaky test ");
    // Type the rest of the message and post.
    fireEvent.change(body, {
      target: { value: "see #Fix the flaky test please" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onPost).toHaveBeenCalledTimes(1);
    expect(onPost.mock.calls[0][0]).toEqual({
      body: "see #Fix the flaky test please",
      references: [{ workItemId: "wi-1", title: "Fix the flaky test" }],
    });
  });

  it("agent `@` mention still works alongside `#` ticket refs in one post", async () => {
    const onPost = vi.fn();
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={onPost} searchMentions={searchMentions} />);
    const body = screen.getByTestId("composer-body") as HTMLTextAreaElement;
    // Pick a ticket via `#`.
    fireEvent.change(body, { target: { value: "#fl" } });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option").length).toBe(1);
    });
    fireEvent.click(screen.getAllByTestId("mention-option")[0]);
    expect(body.value).toBe("#Fix the flaky test ");
    // Then `@`-mention an agent (agents lead the unfiltered `@` list).
    fireEvent.change(body, {
      target: { value: "#Fix the flaky test @am" },
    });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option").length).toBe(2);
    });
    fireEvent.click(screen.getAllByTestId("mention-option")[0]);
    expect(body.value).toBe("#Fix the flaky test @Amelia ");
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onPost.mock.calls[0][0]).toEqual({
      body: "#Fix the flaky test @Amelia",
      references: [{ workItemId: "wi-1", title: "Fix the flaky test" }],
    });
  });

  it("a work_item picked from the `@` list inserts a #Title token (bug fix)", async () => {
    const searchMentions = vi.fn().mockResolvedValue(MENTIONS);
    render(<Composer onPost={vi.fn()} searchMentions={searchMentions} />);
    const body = screen.getByTestId("composer-body") as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: "ping @fl" } });
    await waitFor(() => {
      expect(screen.getAllByTestId("mention-option").length).toBe(2);
    });
    // option[1] is the work_item under the unfiltered `@` list.
    fireEvent.click(screen.getAllByTestId("mention-option")[1]);
    expect(body.value).toBe("ping #Fix the flaky test ");
  });
});
