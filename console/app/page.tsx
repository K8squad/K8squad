// app/page.tsx — console root: the story 8.1 squad overview (ISI-2900).
//
// This route WAS a static scaffold shell; the backend read model (GET /api/squad/overview,
// overview.go) existed but was never fetched. The screen now mounts SquadOverview, which
// renders the caller's Team-scoped Teams→Projects→Run-status projection through the BFF
// (app/api/squad/overview) — the §13 choke point, one authz path, no client-side scoping.
// The 8.x feature screens (run detail, build browser, discussion room, artifact browser at
// /runs/<runId>/artifacts) stay reachable from this overview's Run links.

import { SquadOverview } from "@/components/SquadOverview";

export default function HomePage() {
  return <SquadOverview />;
}
