import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { Composer } from "@/components/discussion/Composer";

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
