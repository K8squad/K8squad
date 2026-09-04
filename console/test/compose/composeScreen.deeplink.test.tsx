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

// E5-S1 (ISI-3685): the kind <select> is replaced by a tab bar (role="tablist"
// aria-label="Compose kind"); the selected kind is the tab with aria-selected="true".
function activeTab() {
  return screen
    .getAllByRole("tab")
    .find((t) => t.getAttribute("aria-selected") === "true");
}

describe("ComposeScreen deep-link seeding", () => {
  it("?kind=agents pre-selects the Agent kind in create mode (AC2)", () => {
    renderWith("kind=agents");
    expect(activeTab()).toHaveTextContent("Agent");
    expect(screen.getByRole("button", { name: "Create" })).toHaveAttribute("aria-pressed", "true");
  });

  it("?kind=agents&mode=edit&name=<n> seeds edit mode with the name pre-filled (AC3)", () => {
    renderWith("kind=agents&mode=edit&name=reviewer");
    expect(activeTab()).toHaveTextContent("Agent");
    expect(screen.getByRole("button", { name: "Edit by name" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect((screen.getByPlaceholderText("my-resource") as HTMLInputElement).value).toBe("reviewer");
  });

  it("a bare mount with no params is unchanged — projects / create (AC4)", () => {
    renderWith("");
    expect(activeTab()).toHaveTextContent("Project");
    expect(screen.getByRole("button", { name: "Create" })).toHaveAttribute("aria-pressed", "true");
  });

  it("an unrecognized ?kind falls back to the default (AC4)", () => {
    renderWith("kind=bogus");
    expect(activeTab()).toHaveTextContent("Project");
  });

  // Regression guard (ISI-3670): the Agent form must mount the guided <ModelSelector> (ISI-3555),
  // not a raw "Model" text input. PR #261 dropped it; the isolated ModelSelector suite stayed green
  // because nothing asserted the picker actually renders inside the mounted AgentForm.
  it("?kind=agents mounts the guided ModelSelector (ISI-3555), not a raw model field", () => {
    renderWith("kind=agents");
    expect(screen.getByRole("button", { name: /bring your own endpoint/i })).toBeInTheDocument();
  });
});
