import { describe, it, expect, vi, afterEach } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { Composer } from "@/components/discussion/Composer";

afterEach(cleanup);

// ISI-5674 WS-D — the `/reset` slash command at the Composer boundary: a leading
// `/` opens the command menu; a resolved command is INTERCEPTED (handed to
// onCommand, never posted); nothing changes when onCommand is unwired.

describe("<Composer> — /reset slash command", () => {
  it("typing a leading `/` opens the command menu with /reset", () => {
    render(<Composer onPost={vi.fn()} onCommand={vi.fn()} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/" },
    });
    expect(screen.getByTestId("slash-popover")).toBeTruthy();
    const opts = screen.getAllByTestId("slash-option");
    expect(opts).toHaveLength(1);
    expect(opts[0].getAttribute("data-command")).toBe("reset");
    expect(screen.getByText("Clear this discussion")).toBeTruthy();
  });

  it("filters the menu by the typed fragment", () => {
    render(<Composer onPost={vi.fn()} onCommand={vi.fn()} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/res" },
    });
    expect(screen.getAllByTestId("slash-option")).toHaveLength(1);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/xyz" },
    });
    expect(screen.getByTestId("slash-empty")).toBeTruthy();
  });

  it("clicking /reset intercepts: onCommand fires, nothing is posted, box clears", () => {
    const onPost = vi.fn();
    const onCommand = vi.fn();
    render(<Composer onPost={onPost} onCommand={onCommand} />);
    const body = screen.getByTestId("composer-body") as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: "/reset" } });
    fireEvent.click(screen.getByTestId("slash-option"));
    expect(onCommand).toHaveBeenCalledWith("reset");
    expect(onPost).not.toHaveBeenCalled();
    expect(body.value).toBe("");
  });

  it("Enter in the menu runs the highlighted command (never a newline/post)", () => {
    const onPost = vi.fn();
    const onCommand = vi.fn();
    render(<Composer onPost={onPost} onCommand={onCommand} />);
    const body = screen.getByTestId("composer-body");
    fireEvent.change(body, { target: { value: "/reset" } });
    fireEvent.keyDown(body, { key: "Enter" });
    expect(onCommand).toHaveBeenCalledWith("reset");
    expect(onPost).not.toHaveBeenCalled();
  });

  it("submitting a bare /reset intercepts instead of posting the literal text", () => {
    const onPost = vi.fn();
    const onCommand = vi.fn();
    render(<Composer onPost={onPost} onCommand={onCommand} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/reset" },
    });
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onCommand).toHaveBeenCalledWith("reset");
    expect(onPost).not.toHaveBeenCalled();
  });

  it("Escape dismisses the command menu", () => {
    render(<Composer onPost={vi.fn()} onCommand={vi.fn()} />);
    const body = screen.getByTestId("composer-body");
    fireEvent.change(body, { target: { value: "/" } });
    expect(screen.getByTestId("slash-popover")).toBeTruthy();
    fireEvent.keyDown(body, { key: "Escape" });
    expect(screen.queryByTestId("slash-popover")).toBeNull();
  });

  it("without onCommand, `/reset` is ordinary text and posts verbatim", () => {
    const onPost = vi.fn();
    render(<Composer onPost={onPost} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/reset" },
    });
    expect(screen.queryByTestId("slash-popover")).toBeNull();
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onPost).toHaveBeenCalledWith({ body: "/reset" });
  });

  it("`/reset now` (argument form) is NOT a command — it posts as text", () => {
    const onPost = vi.fn();
    const onCommand = vi.fn();
    render(<Composer onPost={onPost} onCommand={onCommand} />);
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "/reset now" },
    });
    // menu closes once a space is typed
    expect(screen.queryByTestId("slash-popover")).toBeNull();
    fireEvent.click(screen.getByTestId("composer-submit"));
    expect(onCommand).not.toHaveBeenCalled();
    expect(onPost).toHaveBeenCalledWith({ body: "/reset now" });
  });

  it("does not disturb the @-mention popover for ordinary mention text", async () => {
    const searchMentions = vi
      .fn()
      .mockResolvedValue([{ type: "agent", id: "a1", displayName: "Amelia" }]);
    render(
      <Composer
        onPost={vi.fn()}
        onCommand={vi.fn()}
        searchMentions={searchMentions}
      />,
    );
    fireEvent.change(screen.getByTestId("composer-body"), {
      target: { value: "hi @Am" },
    });
    await waitFor(() =>
      expect(screen.getByTestId("mention-popover")).toBeTruthy(),
    );
    expect(screen.queryByTestId("slash-popover")).toBeNull();
  });
});
