// lib/onboarding.ts — the Launchpad hub model (E1-S2, ISI-3674; frames 01/06/07; FR-1, AD-2).
//
// PURE derivation layer over the server-truth onboarding projection
// (GET /api/onboarding/progress, E1-S1 / internal/apiserver/onboarding.go):
//
//   { step, done, total: 4, nextMilestone?, dismissed? }
//
// The four milestones, in journey order (AD-2):
//   ① team    — the Team CR exists
//   ② agents  — the three preset Roles (role-boss / role-implementer / role-manager) covered
//   ③ models  — every Agent carries a credentialSecretRef (no recorded test failure)
//   ④ project — a Project CR has spec.repo.auth set
//
// Everything the Launchpad renders is derived HERE (states, hero copy, resume target) so the
// component stays a thin renderer and every derivation is unit-tested. Milestone copy mirrors
// the ISI-3641 mock frames 01/06/07 verbatim; hrefs for the "Review" affordance point at the
// list surfaces (E1-S3's ONBOARDING_MILESTONE_HREF in lib/nav.ts is the routing table for the
// chip's *work* surfaces — kept separate so the two stories merge cleanly).

/** Onboarding milestone ids, in journey order — mirror of the Go constants (onboarding.go). */
export type OnboardingMilestoneId = "team" | "agents" | "models" | "project";

/** The AD-2 projection payload (E1-S1). `step` is 1-based first INCOMPLETE milestone (== total
 * at 4/4); `done` counts every complete milestone (out-of-order completion counts); `dismissed`
 * surfaces the Team-CR annotation ksquad.io/onboarding-dismissed. */
export type OnboardingProgress = {
  step: number;
  done: number;
  total: number;
  nextMilestone?: string;
  dismissed?: boolean;
};

/** Card state per the mock: frame 01 (0/4) shows every card "To do"; frame 06 (resume) shows
 * done cards "Done", the first incomplete "Up next", and later incompletes "Locked". */
export type MilestoneState = "done" | "next" | "locked" | "todo";

export type OnboardingMilestone = {
  id: OnboardingMilestoneId;
  /** 1-based journey position (matches the projection's `step`). */
  step: number;
  /** Card title (frames 01/06). */
  title: string;
  /** One-line "why" (FR-1.1). */
  why: string;
  /** Small tag line under the why (frame copy). */
  tag: string;
  /** Spine label (short). */
  spine: string;
  /** "Review" affordance target once done — the milestone's LIST surface. */
  reviewHref: string;
  /** Tag shown on the frame-07 summary card. */
  summaryTag: string;
};

/** The four milestones in journey order, copy aligned to mock frames 01/06/07. */
export const ONBOARDING_MILESTONES: ReadonlyArray<OnboardingMilestone> = [
  {
    id: "team",
    step: 1,
    title: "Create a Team",
    why: "A squad is a Team of agents that work as one unit.",
    tag: "Boss · Implementation · Manager",
    spine: "Team",
    reviewHref: "/teams",
    summaryTag: "1 team",
  },
  {
    id: "agents",
    step: 2,
    title: "Add your agents",
    why: "Start from a starter-squad template, then tune.",
    tag: "3 roles, pre-wired",
    spine: "Agents",
    reviewHref: "/agents",
    summaryTag: "preset squad",
  },
  {
    id: "models",
    step: 3,
    title: "Choose models",
    why: "Pick a primary model + a fallback for each agent.",
    tag: "BYO key or shared",
    spine: "Models",
    reviewHref: "/credentials",
    summaryTag: "primary + fallback",
  },
  {
    id: "project",
    step: 4,
    title: "Connect a Project",
    why: "Point a repo + GitHub token so agents can ship.",
    tag: "repo + secret://token",
    spine: "Project",
    reviewHref: "/projects",
    summaryTag: "repo + secret://token",
  },
];

export const ONBOARDING_TOTAL = ONBOARDING_MILESTONES.length;

/** The seeded preset-Role sequence milestone ② asks for (AD-3 / E2-S1 seeds in config/roles/;
 * the BE derives completion from the same three ids — onboarding.go onboardingPresetRoles).
 * Used to guide the template on-ramp's per-agent suggestion; the Role CRs themselves carry no
 * model (F-CRD-1), so no model defaults live here. */
export const SUGGESTED_AGENT_ROLES: ReadonlyArray<{
  roleRef: string;
  label: string;
  summary: string;
}> = [
  { roleRef: "role-boss", label: "Boss", summary: "Plans, decomposes and delegates." },
  { roleRef: "role-implementer", label: "Implementer", summary: "Writes, tests and ships code." },
  { roleRef: "role-manager", label: "Manager", summary: "Grooms, reviews and unblocks." },
];

/** Type guard: a payload is a well-formed projection (fail-open boundary for the BFF fetch). */
export function isOnboardingProgress(v: unknown): v is OnboardingProgress {
  if (typeof v !== "object" || v === null) return false;
  const p = v as Record<string, unknown>;
  return (
    typeof p.step === "number" &&
    typeof p.done === "number" &&
    typeof p.total === "number" &&
    p.total > 0 &&
    p.done >= 0 &&
    p.done <= p.total
  );
}

/** The first incomplete milestone id, or null at 4/4. Validates against the known ids — an
 * unrecognized `nextMilestone` from a newer server degrades to null (complete-looking) rather
 * than routing Nadia to a surface this console doesn't know. */
export function nextMilestoneId(p: OnboardingProgress): OnboardingMilestoneId | null {
  if (p.done >= p.total) return null;
  const id = p.nextMilestone ?? "";
  return (ONBOARDING_MILESTONES as ReadonlyArray<{ id: string }>).some((m) => m.id === id)
    ? (id as OnboardingMilestoneId)
    : // Server said "incomplete" but named no known milestone — fall back to the step number.
      (ONBOARDING_MILESTONES[p.step - 1]?.id ?? null);
}

/** True when the journey is complete (4/4 — the D5 yield condition). */
export function isJourneyComplete(p: OnboardingProgress): boolean {
  return p.total > 0 && p.done >= p.total;
}

/**
 * Per-card state derivation (frames 01/06):
 *  - milestones before `step` (the first incomplete) → "done" (Review affordance)
 *  - virgin tenant (done == 0): every incomplete card is "todo" (frame 01 — nothing is
 *    "locked" yet; the hero on-ramps are the action, not per-card CTAs)
 *  - resume (done > 0): the FIRST incomplete card is "next" (Resume →), later incompletes are
 *    "locked" (frame 06 — the guided journey is sequential)
 *
 * Payload limit (documented, NFR-5-consistent): the projection carries only the done COUNT and
 * the first-incomplete step, not per-milestone booleans — so a milestone completed OUT OF ORDER
 * (e.g. Project created via Compose before Agents) still counts in the "n of 4" meter but its
 * card renders by journey position ("locked"), not as done. The guided flow is sequential by
 * design (frame 06 locks later cards), so only Compose-direct users can observe this.
 */
export function milestoneStates(
  p: OnboardingProgress,
): Record<OnboardingMilestoneId, MilestoneState> {
  const next = nextMilestoneId(p);
  const virgin = p.done === 0;
  const out = {} as Record<OnboardingMilestoneId, MilestoneState>;
  for (const m of ONBOARDING_MILESTONES) {
    if (p.step > m.step || (next === null && p.done >= p.total)) {
      // `step` is the first INCOMPLETE milestone: anything before it is complete. At 4/4
      // (next === null, step === total) every milestone is done.
      out[m.id] = "done";
    } else if (virgin) {
      out[m.id] = "todo";
    } else if (m.id === next) {
      out[m.id] = "next";
    } else {
      out[m.id] = "locked";
    }
  }
  return out;
}

/**
 * Hero copy per frame. done == 0 → frame 01 ("Welcome to K8squad"); 0 < done < total →
 * frame 06 ("Finish setting up your squad"); done == total → frame 07 ("Your squad is ready").
 */
export function heroCopy(p: OnboardingProgress): { title: string; sub: string } {
  if (isJourneyComplete(p)) {
    return {
      title: "Your squad is ready",
      sub: "Your squad is live — agents composed, models wired, and a project connected.",
    };
  }
  if (p.done === 0) {
    return {
      title: "Welcome to K8squad",
      sub: "Your tenant is empty. Let's assemble your first squad — four quick steps.",
    };
  }
  const left = p.total - p.done;
  const headline = p.done * 2 === p.total ? "You're halfway there" : `${p.done} of ${p.total} complete`;
  return {
    title: "Finish setting up your squad",
    sub: `${headline} — ${left} step${left === 1 ? "" : "s"} left. Pick up where you left off.`,
  };
}

/** The resume CTA label for the frame-06 hero ("Resume setup — Choose models →"). */
export function resumeLabel(p: OnboardingProgress): string {
  const next = nextMilestoneId(p);
  const title = ONBOARDING_MILESTONES.find((m) => m.id === next)?.title;
  return title ? `Resume setup — ${title}` : "Resume setup";
}

/**
 * D5 gate: should the Overview route render the Launchpad instead of SquadOverview?
 * Show while the journey is incomplete AND not dismissed (server annotation OR this device's
 * localStorage flag — FR-1.3). Complete journeys always yield to the normal Overview.
 */
export function shouldShowLaunchpad(
  p: OnboardingProgress,
  localDismissed: boolean,
): boolean {
  if (isJourneyComplete(p)) return false;
  return !(p.dismissed === true || localDismissed);
}

// ── Local dismissal (FR-1.3 v1 floor) ────────────────────────────────────────
// The server projection surfaces `dismissed` from the Team-CR annotation, but E1-S1 ships no
// HTTP write-path for it (the SetOnboardingDismissed helper exists; the route does not). Until
// that lands (follow-up), "Skip for now" persists on THIS device via localStorage; the server
// flag is honored first whenever present, so the cross-device flag wins the moment a writer
// exists. Dismissal is a UI convenience, never progress (E1-S1 AC3) — the derived counts stay
// authoritative.

export const ONBOARDING_DISMISS_KEY = "ksquad.onboarding.dismissed";

/** Read this device's dismissal flag (false outside the browser / on storage errors). */
export function readLocalDismissed(storage?: Pick<Storage, "getItem">): boolean {
  try {
    const s = storage ?? (typeof window !== "undefined" ? window.localStorage : undefined);
    return s?.getItem(ONBOARDING_DISMISS_KEY) === "true";
  } catch {
    return false;
  }
}

/** Persist (or clear, when false) this device's dismissal flag. */
export function writeLocalDismissed(
  dismissed: boolean,
  storage?: Pick<Storage, "setItem" | "removeItem">,
): void {
  try {
    const s = storage ?? (typeof window !== "undefined" ? window.localStorage : undefined);
    if (!s) return;
    if (dismissed) s.setItem(ONBOARDING_DISMISS_KEY, "true");
    else s.removeItem(ONBOARDING_DISMISS_KEY);
  } catch {
    /* storage unavailable (private mode quota) — dismissal simply doesn't persist */
  }
}
