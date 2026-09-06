// test/onboarding/Launchpad.test.tsx — the Launchpad hub component (E1-S2, ISI-3674;
// frames 01/06/07; AC1–AC6).

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { Launchpad } from "@/components/onboarding/Launchpad";
import type { OnboardingProgress } from "@/lib/onboarding";

afterEach(cleanup);

const virgin: OnboardingProgress = { step: 1, done: 0, total: 4, nextMilestone: "team" };
const halfway: OnboardingProgress = {
  step: 3,
  done: 2,
  total: 4,
  nextMilestone: "models",
  dismissed: false,
};
const complete: OnboardingProgress = { step: 4, done: 4, total: 4, dismissed: false };

const noop = () => {};

beforeEach(() => {
  // ReadyState best-effort squad fetch + any in-session refetch: default to a quiet failure
  // (fail-open paths must not crash the hub).
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 501, text: () => Promise.resolve("") }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("frame 01 — empty tenant (AC1/AC2)", () => {
  it("renders the welcome hero, meter, spine and four todo milestone cards", () => {
    render(<Launchpad initialProgress={virgin} onDismiss={noop} onYield={noop} />);

    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Welcome to K8squad");
    const meter = screen.getByRole("progressbar", {
      name: "Setup progress: 0 of 4 complete",
    });
    expect(meter).toHaveAttribute("aria-valuenow", "0");
    expect(meter).toHaveAttribute("aria-valuemax", "4");

    for (const title of ["Create a Team", "Add your agents", "Choose models", "Connect a Project"]) {
      expect(screen.getByRole("heading", { name: title })).toBeTruthy();
    }
    expect(screen.getAllByText("To do")).toHaveLength(4);
    // One-line why per milestone (FR-1.1).
    expect(
      screen.getByText("A squad is a Team of agents that work as one unit."),
    ).toBeTruthy();
  });

  it("presents the two on-ramps as the first choice (AC2, FR-1.2)", () => {
    render(<Launchpad initialProgress={virgin} onDismiss={noop} onYield={noop} />);
    const template = screen.getByRole("button", { name: /Start from a starter squad/i });
    const manual = screen.getByRole("button", { name: /Build it step by step/i });
    expect(template.textContent).toContain("recommended");
    expect(manual).toBeTruthy();
    expect(screen.getByText(/you can pause and resume anytime/)).toBeTruthy();
  });

  it("Skip for now dismisses the hub (FR-1.3)", () => {
    const onDismiss = vi.fn();
    render(<Launchpad initialProgress={virgin} onDismiss={onDismiss} onYield={noop} />);
    fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("an on-ramp opens the next milestone's step panel mounting the E0 shared form (AC3, FR-8)", () => {
    render(<Launchpad initialProgress={virgin} onDismiss={noop} onYield={noop} />);
    fireEvent.click(screen.getByRole("button", { name: /Start from a starter squad/i }));
    // Team step: the E0 TeamForm fields (name + namespace strategy), not bespoke inputs.
    const panel = screen.getByRole("region", { name: /Create a Team — setup step 1 of 4/i });
    expect(panel.querySelector('input[placeholder="my-resource"]')).toBeTruthy();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
  });
});

describe("frame 06 — resume (AC3)", () => {
  it("renders the resume hero with the next milestone named", () => {
    render(<Launchpad initialProgress={halfway} onDismiss={noop} onYield={noop} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(
      "Finish setting up your squad",
    );
    expect(
      screen.getByRole("button", { name: /Resume setup — Choose models/i }),
    ).toBeTruthy();
    expect(screen.getByText(/step 3 of 4/)).toBeTruthy();
  });

  it("cards show Done / Done / Up next / Locked with a Review affordance on done cards", () => {
    render(<Launchpad initialProgress={halfway} onDismiss={noop} onYield={noop} />);
    expect(screen.getAllByText("Done")).toHaveLength(2);
    expect(screen.getByText("Up next")).toBeTruthy();
    const lockedChip = document.querySelector(".launchpad-card__state--locked");
    expect(lockedChip).toBeTruthy();
    expect(lockedChip!.textContent).toBe("Locked");
    const reviews = screen.getAllByRole("link", { name: "Review" });
    expect(reviews.map((r) => r.getAttribute("href"))).toEqual(["/teams", "/agents"]);
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "2");
  });

  it("the next milestone's card CTA opens its step (models routes to /credentials)", () => {
    render(<Launchpad initialProgress={halfway} onDismiss={noop} onYield={noop} />);
    fireEvent.click(screen.getByRole("button", { name: "Resume →" }));
    const panel = screen.getByRole("region", { name: /Choose models — setup step 3 of 4/i });
    expect(panel).toBeTruthy();
    expect(screen.getByRole("link", { name: /Open Credentials/i })).toHaveAttribute(
      "href",
      "/credentials",
    );
  });
});

describe("frame 07 — ready state (AC4, FR-1.5)", () => {
  it("shows 'Your squad is ready' with the two exit CTAs", async () => {
    render(<Launchpad initialProgress={complete} onDismiss={noop} onYield={noop} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Your squad is ready");
    expect(screen.getByRole("button", { name: /Go to Overview/i })).toBeTruthy();
    expect(screen.getByRole("link", { name: /Open Compose/i })).toHaveAttribute(
      "href",
      "/compose",
    );
    // Summary cards carry the four milestones.
    for (const label of ["Team", "Agents", "Models", "Project"]) {
      expect(screen.getByRole("heading", { name: label })).toBeTruthy();
    }
    await waitFor(() => expect(fetch).toHaveBeenCalledWith("/api/squad/overview", expect.anything()));
  });

  it("Go to Overview yields to the normal Overview (AC5)", () => {
    const onYield = vi.fn();
    render(<Launchpad initialProgress={complete} onDismiss={noop} onYield={onYield} />);
    fireEvent.click(screen.getByRole("button", { name: /Go to Overview/i }));
    expect(onYield).toHaveBeenCalledTimes(1);
  });
});

describe("in-session completion (AC3 → AC4)", () => {
  it("creating the final milestone's object flips the hub to the ready state", async () => {
    const atProject: OnboardingProgress = {
      step: 4,
      done: 3,
      total: 4,
      nextMilestone: "project",
      dismissed: false,
    };
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url === "/api/onboarding/progress") {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ step: 4, done: 4, total: 4, dismissed: false }),
        });
      }
      if (url === "/api/compose/projects") {
        return Promise.resolve({
          ok: true,
          json: () =>
            Promise.resolve({
              kind: "projects",
              name: "checkout-service",
              namespace: "team-a",
              revision: 1,
              operation: "created",
            }),
        });
      }
      return Promise.resolve({ ok: false, status: 501, text: () => Promise.resolve("") });
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<Launchpad initialProgress={atProject} onDismiss={noop} onYield={noop} />);
    fireEvent.click(screen.getByRole("button", { name: /Resume setup — Connect a Project/i }));

    const panel = screen.getByRole("region", { name: /Connect a Project — setup step 4 of 4/i });
    const [nameInput] = panel.querySelectorAll('input[placeholder="my-resource"]');
    const repoInput = panel.querySelector(
      'input[placeholder="https://github.com/org/repo"]',
    ) as HTMLInputElement;
    fireEvent.change(nameInput, { target: { value: "checkout-service" } });
    fireEvent.change(repoInput, { target: { value: "https://github.com/acme/checkout" } });

    const create = screen.getByRole("button", { name: "Create" });
    expect(create).not.toBeDisabled();
    fireEvent.click(create);

    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(
        "Your squad is ready",
      ),
    );
  });
});

describe("a11y (AC6, NFR-4)", () => {
  it("marks the next spine node as the current step", () => {
    render(<Launchpad initialProgress={halfway} onDismiss={noop} onYield={noop} />);
    const current = document.querySelector('[aria-current="step"]');
    expect(current).toBeTruthy();
    expect(current!.textContent).toContain("Models");
  });

  it("locked cards carry a text equivalent, never color-only", () => {
    render(<Launchpad initialProgress={halfway} onDismiss={noop} onYield={noop} />);
    const locked = document.querySelector(".launchpad-card--locked");
    expect(locked).toHaveAttribute("aria-disabled", "true");
    expect(locked!.textContent).toContain("Locked");
  });
});
