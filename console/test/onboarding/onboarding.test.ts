// test/onboarding/onboarding.test.ts — the Launchpad derivation layer (E1-S2, ISI-3674;
// frames 01/06/07; FR-1). Pure functions over the E1-S1 server projection.

import { describe, it, expect } from "vitest";
import {
  ONBOARDING_DISMISS_KEY,
  ONBOARDING_MILESTONES,
  ONBOARDING_TOTAL,
  heroCopy,
  isJourneyComplete,
  isOnboardingProgress,
  milestoneStates,
  nextMilestoneId,
  readLocalDismissed,
  resumeLabel,
  shouldShowLaunchpad,
  writeLocalDismissed,
  type OnboardingProgress,
} from "@/lib/onboarding";

const virgin: OnboardingProgress = { step: 1, done: 0, total: 4, nextMilestone: "team" };
const halfway: OnboardingProgress = {
  step: 3,
  done: 2,
  total: 4,
  nextMilestone: "models",
  dismissed: false,
};
const complete: OnboardingProgress = { step: 4, done: 4, total: 4, dismissed: false };

describe("isOnboardingProgress (fail-open payload guard)", () => {
  it("accepts a well-formed projection", () => {
    expect(isOnboardingProgress(virgin)).toBe(true);
    expect(isOnboardingProgress(complete)).toBe(true);
  });

  it("rejects malformed payloads", () => {
    expect(isOnboardingProgress(null)).toBe(false);
    expect(isOnboardingProgress({})).toBe(false);
    expect(isOnboardingProgress({ step: 1, done: 0 })).toBe(false);
    expect(isOnboardingProgress({ step: 1, done: 5, total: 4 })).toBe(false);
    expect(isOnboardingProgress({ step: "1", done: 0, total: 4 })).toBe(false);
  });
});

describe("nextMilestoneId", () => {
  it("names the first incomplete milestone", () => {
    expect(nextMilestoneId(virgin)).toBe("team");
    expect(nextMilestoneId(halfway)).toBe("models");
  });

  it("is null at 4/4", () => {
    expect(nextMilestoneId(complete)).toBeNull();
  });

  it("falls back to the step number for an unrecognized server id", () => {
    const p: OnboardingProgress = { step: 2, done: 1, total: 4, nextMilestone: "billing" };
    expect(nextMilestoneId(p)).toBe("agents");
  });
});

describe("milestoneStates (frames 01/06)", () => {
  it("frame 01 (0/4): every card is 'todo' — nothing locked for a virgin tenant", () => {
    expect(milestoneStates(virgin)).toEqual({
      team: "todo",
      agents: "todo",
      models: "todo",
      project: "todo",
    });
  });

  it("frame 06 (2/4): done · done · up-next · locked", () => {
    expect(milestoneStates(halfway)).toEqual({
      team: "done",
      agents: "done",
      models: "next",
      project: "locked",
    });
  });

  it("1/4 resume: only the next card is actionable, later ones locked", () => {
    const p: OnboardingProgress = { step: 2, done: 1, total: 4, nextMilestone: "agents" };
    expect(milestoneStates(p)).toEqual({
      team: "done",
      agents: "next",
      models: "locked",
      project: "locked",
    });
  });

  it("4/4: every card done", () => {
    expect(milestoneStates(complete)).toEqual({
      team: "done",
      agents: "done",
      models: "done",
      project: "done",
    });
  });
});

describe("heroCopy", () => {
  it("frame 01 welcomes the empty tenant", () => {
    expect(heroCopy(virgin).title).toBe("Welcome to K8squad");
    expect(heroCopy(virgin).sub).toContain("four quick steps");
  });

  it("frame 06 resume copy counts the steps left (halfway phrasing at 2/4)", () => {
    const c = heroCopy(halfway);
    expect(c.title).toBe("Finish setting up your squad");
    expect(c.sub).toBe("You're halfway there — 2 steps left. Pick up where you left off.");
  });

  it("non-halfway resume uses the n/m phrasing + singular step", () => {
    const p: OnboardingProgress = { step: 4, done: 3, total: 4, nextMilestone: "project" };
    expect(heroCopy(p).sub).toBe("3 of 4 complete — 1 step left. Pick up where you left off.");
  });

  it("frame 07 celebrates completion", () => {
    expect(heroCopy(complete).title).toBe("Your squad is ready");
  });
});

describe("resumeLabel (frame 06 hero CTA)", () => {
  it("names the next milestone's title", () => {
    expect(resumeLabel(halfway)).toBe("Resume setup — Choose models");
    expect(resumeLabel(virgin)).toBe("Resume setup — Create a Team");
  });
});

describe("shouldShowLaunchpad (D5 gate, FR-1.3)", () => {
  it("shows while incomplete and not dismissed", () => {
    expect(shouldShowLaunchpad(virgin, false)).toBe(true);
    expect(shouldShowLaunchpad(halfway, false)).toBe(true);
  });

  it("yields at 4/4 (AC5)", () => {
    expect(shouldShowLaunchpad(complete, false)).toBe(false);
  });

  it("hides on server dismissal (Team annotation)", () => {
    expect(shouldShowLaunchpad({ ...halfway, dismissed: true }, false)).toBe(false);
  });

  it("hides on this device's local dismissal", () => {
    expect(shouldShowLaunchpad(halfway, true)).toBe(false);
  });
});

describe("isJourneyComplete", () => {
  it("is true only at done == total", () => {
    expect(isJourneyComplete(complete)).toBe(true);
    expect(isJourneyComplete(halfway)).toBe(false);
    expect(isJourneyComplete(virgin)).toBe(false);
  });
});

describe("local dismissal storage (FR-1.3 v1 floor)", () => {
  function fakeStorage() {
    const map = new Map<string, string>();
    return {
      getItem: (k: string) => map.get(k) ?? null,
      setItem: (k: string, v: string) => void map.set(k, v),
      removeItem: (k: string) => void map.delete(k),
    };
  }

  it("round-trips the flag", () => {
    const s = fakeStorage();
    expect(readLocalDismissed(s)).toBe(false);
    writeLocalDismissed(true, s);
    expect(readLocalDismissed(s)).toBe(true);
    expect(s.getItem(ONBOARDING_DISMISS_KEY)).toBe("true");
    writeLocalDismissed(false, s);
    expect(readLocalDismissed(s)).toBe(false);
  });

  it("fails soft when storage throws", () => {
    const throwing = {
      getItem: () => {
        throw new Error("denied");
      },
    };
    expect(readLocalDismissed(throwing)).toBe(false);
  });
});

describe("milestone metadata", () => {
  it("declares the four AD-2 milestones in journey order", () => {
    expect(ONBOARDING_MILESTONES.map((m) => m.id)).toEqual([
      "team",
      "agents",
      "models",
      "project",
    ]);
    expect(ONBOARDING_TOTAL).toBe(4);
    expect(ONBOARDING_MILESTONES.map((m) => m.step)).toEqual([1, 2, 3, 4]);
  });

  it("carries a why + review surface per milestone (FR-1.1)", () => {
    for (const m of ONBOARDING_MILESTONES) {
      expect(m.why.length).toBeGreaterThan(0);
      expect(m.reviewHref.startsWith("/")).toBe(true);
    }
  });
});
