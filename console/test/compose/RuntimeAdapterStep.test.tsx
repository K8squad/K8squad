// test/compose/RuntimeAdapterStep.test.tsx — ISI-6380 new adapter branches (vibe + goose).
// RuntimeAdapterStep takes only { adapter, onAdapterChange, children } so it renders
// standalone with no fetch stubbing.

import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { RuntimeAdapterStep } from "@/components/compose/RuntimeAdapterStep";

afterEach(cleanup);

describe("<RuntimeAdapterStep> — vibe + goose branches (ISI-6380)", () => {
  it("goose is preview-only: a disabled CTA, its own tint, and no text input", () => {
    render(
      <RuntimeAdapterStep adapter="goose" onAdapterChange={() => {}}>
        <div data-testid="ms" />
      </RuntimeAdapterStep>,
    );
    const cta = screen.getByTestId("goose-preview-cta") as HTMLButtonElement;
    expect(cta.disabled).toBe(true);
    expect(
      document.querySelector(".runtime-adapter")!.getAttribute("style"),
    ).toContain("--adapter-accent");
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("vibe renders the manual note when no vibeSlot is supplied (fail-open contract)", () => {
    render(
      <RuntimeAdapterStep adapter="vibe" onAdapterChange={() => {}}>
        <div />
      </RuntimeAdapterStep>,
    );
    expect(screen.getByTestId("vibe-manual-note")).toBeTruthy();
  });

  it("vibe renders the vibeSlot content when one is supplied", () => {
    render(
      <RuntimeAdapterStep
        adapter="vibe"
        onAdapterChange={() => {}}
        vibeSlot={<div data-testid="vibe-slot-content">slot</div>}
      >
        <div />
      </RuntimeAdapterStep>,
    );
    expect(screen.getByTestId("vibe-slot-content")).toBeTruthy();
    expect(screen.queryByTestId("vibe-cred-note")).toBeNull();
  });
});

describe("<RuntimeAdapterStep> — claude branch (ISI-6702)", () => {
  it("renders the pointer note when no claudeSlot is supplied (fail-open contract)", () => {
    render(
      <RuntimeAdapterStep adapter="claude" onAdapterChange={() => {}}>
        <div />
      </RuntimeAdapterStep>,
    );
    expect(screen.getByTestId("claude-cred-note")).toBeTruthy();
    expect(screen.queryByTestId("creds-create-form")).toBeNull();
  });

  it("renders the claudeSlot content when one is supplied, and the OAuth CTA stays disabled", () => {
    render(
      <RuntimeAdapterStep
        adapter="claude"
        onAdapterChange={() => {}}
        claudeSlot={<div data-testid="claude-slot-content">slot</div>}
      >
        <div />
      </RuntimeAdapterStep>,
    );
    expect(screen.getByTestId("claude-slot-content")).toBeTruthy();
    expect(screen.queryByTestId("claude-cred-note")).toBeNull();
    // The subscription path is ToS-gated (ISI-3661): never a live OAuth click.
    const cta = screen.getByTestId("claude-oauth-cta") as HTMLButtonElement;
    expect(cta.disabled).toBe(true);
  });
});
