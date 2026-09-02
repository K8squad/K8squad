// test/compose/composeScreen.deeplink.test.tsx — ComposeScreen honors deep-link params (ISI-3554).
//
// Companion to deeplink.test.ts (pure param mapping): this renders the real ComposeScreen and proves
// the params actually seed the visible form — kind pre-selected, mode pre-pressed, name pre-filled —
// and that a bare mount (no params) is unchanged (AC2/AC3/AC4). It stubs next/navigation's
// useSearchParams (jsdom has no App Router), the same seam the component reads.

import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { ComposeScreen } from "@/components/compose/ComposeScreen";

let current = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useSearchParams: () => current,
}));

afterEach(() => {
  cleanup();
  current = new URLSearchParams();
});

function renderWith(query: string) {
  current = new URLSearchParams(query);
  return render(<ComposeScreen />);
}

describe("ComposeScreen deep-link seeding", () => {
  it("?kind=agents pre-selects the Agent kind in create mode (AC2)", () => {
    renderWith("kind=agents");
    expect((screen.getByLabelText("Compose kind") as HTMLSelectElement).value).toBe("agents");
    expect(screen.getByRole("button", { name: "Create" })).toHaveAttribute("aria-pressed", "true");
  });

  it("?kind=agents&mode=edit&name=<n> seeds edit mode with the name pre-filled (AC3)", () => {
    renderWith("kind=agents&mode=edit&name=reviewer");
    expect((screen.getByLabelText("Compose kind") as HTMLSelectElement).value).toBe("agents");
    expect(screen.getByRole("button", { name: "Edit by name" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect((screen.getByPlaceholderText("my-resource") as HTMLInputElement).value).toBe("reviewer");
  });

  it("a bare mount with no params is unchanged — projects / create (AC4)", () => {
    renderWith("");
    expect((screen.getByLabelText("Compose kind") as HTMLSelectElement).value).toBe("projects");
    expect(screen.getByRole("button", { name: "Create" })).toHaveAttribute("aria-pressed", "true");
  });

  it("an unrecognized ?kind falls back to the default (AC4)", () => {
    renderWith("kind=bogus");
    expect((screen.getByLabelText("Compose kind") as HTMLSelectElement).value).toBe("projects");
  });
});
