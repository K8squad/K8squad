// app/(app)/instscore/page.tsx — the Instrumentation Score route (ISI-7750,
// phase 4 of the ISI-7741 plan).
//
// Mounted inside the Epic 8 shell (app/layout.tsx — theming, topbar; the
// (app) group adds the authenticated shell + nav rail). Thin by design: the
// client screen owns fetch/state so every render path (ok / empty / denied /
// unconfigured 501 / error) is testable at the component boundary — the same
// shape as the audit trail route.

import { InstScoreScreen } from "@/components/instscore/InstScoreScreen";

export const metadata = {
  title: "Instrumentation Score — K8squad Console",
};

export default function InstScorePage() {
  return <InstScoreScreen />;
}
