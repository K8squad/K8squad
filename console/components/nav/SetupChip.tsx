"use client";

// components/nav/SetupChip.tsx — the persistent "Finish setup (n/4)" chip (E1-S3 / ISI-3675,
// AD-10, FR-1.3). Returning "Raj" always has a way back to setup: while the journey is
// incomplete AND the Launchpad has been dismissed (E1-S2 owns the not-yet-dismissed state),
// this chip rides at the foot of the nav rail and inside the mobile drawer, reading the AD-2
// progress projection (GET /api/onboarding/progress, E1-S1) and routing to the NEXT
// milestone's surface. It disappears at 4/4 (AC4) and never renders for a tenant whose
// Launchpad is still showing (the Launchpad itself is the way back then, AC2).

import Link from "next/link";
import { NavIcon } from "@/components/nav/NavIcon";
import {
  ONBOARDING_MILESTONE_HREF,
  type OnboardingProgress,
} from "@/lib/nav";

export function SetupChip({
  progress,
  onNavigate,
}: {
  progress: OnboardingProgress | null;
  onNavigate?: () => void;
}) {
  // AC2/AC4: only while setup is incomplete AND the Launchpad was dismissed. A null
  // progress (endpoint unreachable or not yet loaded) fails open to no chip — the chip is
  // a convenience, never a gate.
  if (!progress) return null;
  if (progress.total <= 0 || progress.done >= progress.total) return null;
  if (!progress.dismissed) return null;

  const milestone = progress.nextMilestone ?? "";
  const href = ONBOARDING_MILESTONE_HREF[milestone] ?? "/overview";
  return (
    <Link
      href={href}
      className="setupchip"
      data-testid="setup-chip"
      data-milestone={milestone || undefined}
      onClick={onNavigate}
      aria-label={`Finish setup, ${progress.done} of ${progress.total} milestones complete`}
    >
      <span className="setupchip__icon" aria-hidden="true">
        <NavIcon id="build" size={15} />
      </span>
      <span className="setupchip__label">
        Finish setup ({progress.done}/{progress.total})
      </span>
    </Link>
  );
}
