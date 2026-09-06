// app/page.tsx — console root: the story 8.1 squad overview (ISI-2900).
//
// This route WAS a static scaffold shell; the backend read model (GET /api/squad/overview,
// overview.go) existed but was never fetched. The screen mounts the SAME D5-gated surface as
// /overview (E1-S2, ISI-3674): the Launchpad hub while setup is incomplete, the caller's
// Team-scoped Teams→Projects→Run-status projection (SquadOverview, through the BFF — the §13
// choke point, one authz path, no client-side scoping) once setup completes or the projection
// is unavailable (fail-open). The 8.x feature screens (run detail, build browser, discussion
// room, artifact browser at /runs/<runId>/artifacts) stay reachable from this overview's Run
// links.

import { OverviewSwitch } from "@/components/onboarding/OverviewSwitch";

export default function HomePage() {
  return <OverviewSwitch />;
}
