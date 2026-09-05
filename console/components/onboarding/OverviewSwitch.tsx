"use client";

// components/onboarding/OverviewSwitch.tsx — the D5 gate (E1-S2, ISI-3674, AC5).
//
// "The Launchpad REPLACES Overview until complete." This component owns that decision for the
// Overview route(s): it reads the server-truth projection (GET /api/onboarding/progress,
// E1-S1) once per mount and renders either the Launchpad (setup incomplete, not dismissed) or
// the normal SquadOverview.
//
// Fail-open (same discipline as E1-S3's nav lock): if the endpoint is unreachable,
// unauthenticated, or not yet wired (the E1-S1 apiserver route still landing), the route
// renders the normal Overview — the Launchpad is an on-ramp, never a gate that strands users.
//
// Dismissal (FR-1.3): "Skip for now" persists on this device (localStorage — the server's
// annotation write-path is a follow-up; the projection's `dismissed` flag is honored first
// whenever a writer exists). While dismissed, the Overview renders with a slim "Finish setup
// (n/4)" resume banner — a way back that works even before E1-S3's nav chip activates (the
// chip reads the SERVER flag, which v1 cannot set).
//
// Yield (AC5): at 4/4 the normal Overview renders. The in-session celebration (frame 07) lives
// inside the Launchpad; its "Go to Overview" CTA flips `yielded` here so the same mount swaps
// to SquadOverview without a navigation.

import { useEffect, useState } from "react";
import { SquadOverview } from "@/components/SquadOverview";
import { Launchpad } from "@/components/onboarding/Launchpad";
import {
  isJourneyComplete,
  isOnboardingProgress,
  readLocalDismissed,
  shouldShowLaunchpad,
  writeLocalDismissed,
  type OnboardingProgress,
} from "@/lib/onboarding";

export function OverviewSwitch() {
  const [progress, setProgress] = useState<OnboardingProgress | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [localDismissed, setLocalDismissed] = useState(false);
  const [yielded, setYielded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLocalDismissed(readLocalDismissed());
    fetch("/api/onboarding/progress", { cache: "no-store" })
      .then((res) => (res.ok ? res.json() : null))
      .then((p: unknown) => {
        if (cancelled) return;
        if (isOnboardingProgress(p)) setProgress(p);
        setLoaded(true);
      })
      .catch(() => {
        if (!cancelled) setLoaded(true);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!loaded) {
    return (
      <div className="launchpad-loading" aria-busy="true">
        <span className="muted">Loading…</span>
      </div>
    );
  }

  if (progress && !yielded && shouldShowLaunchpad(progress, localDismissed)) {
    return (
      <Launchpad
        initialProgress={progress}
        onDismiss={() => {
          writeLocalDismissed(true);
          setLocalDismissed(true);
        }}
        onYield={() => setYielded(true)}
      />
    );
  }

  // Suppressed-by-local-dismissal: normal Overview + the way back (FR-1.3 v1 floor). When the
  // SERVER flag is the dismisser, E1-S3's nav chip owns the way back (it routes to the next
  // milestone's work surface), so no banner here.
  const showResumeBanner =
    progress !== null &&
    !isJourneyComplete(progress) &&
    localDismissed &&
    progress.dismissed !== true;

  return (
    <>
      {showResumeBanner && (
        <button
          type="button"
          className="launchpad__banner"
          onClick={() => {
            writeLocalDismissed(false);
            setLocalDismissed(false);
          }}
        >
          Finish setup ({progress.done}/{progress.total}) — Resume →
        </button>
      )}
      <SquadOverview />
    </>
  );
}
