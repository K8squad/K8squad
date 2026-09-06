// test/onboarding/OverviewSwitch.test.tsx — the D5 gate (E1-S2, ISI-3674, AC5, FR-1.3):
// Launchpad replaces Overview until complete; fail-open when the projection is unavailable;
// dismissal persists on-device with a resume banner.

import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { OverviewSwitch } from "@/components/onboarding/OverviewSwitch";
import { ONBOARDING_DISMISS_KEY } from "@/lib/onboarding";

afterEach(cleanup);

function mockFetch(handler: (url: string) => Promise<unknown>) {
  vi.stubGlobal("fetch", vi.fn().mockImplementation(handler));
}

const progressOk = (p: unknown) =>
  Promise.resolve({ ok: true, json: () => Promise.resolve(p) });
const notWired = () =>
  Promise.resolve({ ok: false, status: 501, text: () => Promise.resolve("") });

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("D5 gate", () => {
  it("renders the Launchpad while setup is incomplete (AC5)", async () => {
    mockFetch((url) =>
      url === "/api/onboarding/progress"
        ? progressOk({ step: 1, done: 0, total: 4, nextMilestone: "team" })
        : notWired(),
    );
    render(<OverviewSwitch />);
    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(
        "Welcome to K8squad",
      ),
    );
  });

  it("yields to the normal Overview at 4/4 (AC5)", async () => {
    mockFetch((url) =>
      url === "/api/onboarding/progress"
        ? progressOk({ step: 4, done: 4, total: 4, dismissed: false })
        : notWired(),
    );
    render(<OverviewSwitch />);
    await waitFor(() =>
      expect(screen.queryByText("Welcome to K8squad")).toBeNull(),
    );
    expect(screen.queryByText("Your squad is ready")).toBeNull();
  });

  it("fails open to the normal Overview when the projection is unavailable", async () => {
    mockFetch(() => Promise.reject(new Error("network down")));
    render(<OverviewSwitch />);
    await waitFor(() =>
      expect(screen.queryByText("Welcome to K8squad")).toBeNull(),
    );
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("honors the server dismissal flag without a banner (E1-S3's chip owns the way back)", async () => {
    mockFetch((url) =>
      url === "/api/onboarding/progress"
        ? progressOk({ step: 3, done: 2, total: 4, nextMilestone: "models", dismissed: true })
        : notWired(),
    );
    render(<OverviewSwitch />);
    await waitFor(() =>
      expect(screen.queryByText("Finish setting up your squad")).toBeNull(),
    );
    expect(screen.queryByRole("button", { name: /Finish setup/i })).toBeNull();
  });
});

describe("dismissal (FR-1.3 v1 floor)", () => {
  const halfway = { step: 3, done: 2, total: 4, nextMilestone: "models", dismissed: false };

  it("Skip for now persists on-device, swaps to Overview + resume banner", async () => {
    mockFetch((url) =>
      url === "/api/onboarding/progress" ? progressOk(halfway) : notWired(),
    );
    render(<OverviewSwitch />);
    await waitFor(() => screen.getByText("Finish setting up your squad"));

    fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));

    expect(window.localStorage.getItem(ONBOARDING_DISMISS_KEY)).toBe("true");
    expect(screen.queryByText("Finish setting up your squad")).toBeNull();
    const banner = screen.getByRole("button", { name: /Finish setup \(2\/4\)/i });
    expect(banner).toBeTruthy();
  });

  it("the resume banner returns to the Launchpad and clears the flag", async () => {
    window.localStorage.setItem(ONBOARDING_DISMISS_KEY, "true");
    mockFetch((url) =>
      url === "/api/onboarding/progress" ? progressOk(halfway) : notWired(),
    );
    render(<OverviewSwitch />);

    const banner = await waitFor(() =>
      screen.getByRole("button", { name: /Finish setup \(2\/4\)/i }),
    );
    fireEvent.click(banner);

    expect(window.localStorage.getItem(ONBOARDING_DISMISS_KEY)).toBeNull();
    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(
        "Finish setting up your squad",
      ),
    );
  });
});
