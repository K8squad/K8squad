// app/overview/page.tsx — Overview (squad status/activity), the story 8.1 GLOBAL surface.
//
// D5 (E1-S2, ISI-3674): while a tenant's setup is incomplete and not dismissed, this route
// renders the Launchpad hub (frames 01/06/07) INSTEAD of the squad overview; once the journey
// completes it yields back to the normal Overview. OverviewSwitch owns that decision against
// the server-truth projection (GET /api/onboarding/progress, E1-S1) and fails open to the
// SquadOverview when the projection is unavailable — the same live read-model screen as the
// console root (app/page.tsx), with a distinct honest state for every terminal HTTP status
// (401/404/501/5xx). No scaffold/story-scaffold copy reaches the UI.

import { OverviewSwitch } from "@/components/onboarding/OverviewSwitch";
import Link from "next/link";
import { useState, useEffect } from "react";

export default function OverviewPage() {
  const [isMounted, setIsMounted] = useState(false);

  useEffect(() => {
    setIsMounted(true);
  }, []);

  return (
    <div>
      {isMounted && (
        <nav className="ov-band ov-band--padded ov-band--border-b ov-mb-xl">
          <div className="ov-container">
            <div className="ov-flex ov-flex--align-center ov-flex--justify-between">
              <h1 className="ov-heading ov-heading--l">Overview</h1>
              <div className="ov-tabs">
                <Link href="/overview" className="ov-tab ov-tab--active">
                  Squad
                </Link>
                <Link href="/overview/fleet" className="ov-tab">
                  Fleet Control Room
                </Link>
              </div>
            </div>
          </div>
        </nav>
      )}
      <OverviewSwitch />
    </div>
  );
}
