// app/overview/page.tsx — Overview (squad status/activity), the story 8.1 GLOBAL surface.
//
// This route mounts the SAME live read-model screen as the console root (app/page.tsx):
// SquadOverview fetches GET /api/squad/overview through the BFF and renders the caller's
// Team-scoped Teams→Projects→Run-status projection, with a distinct honest state for every
// terminal HTTP status (401/404/501/5xx). No scaffold/story-scaffold copy reaches the UI.

import { SquadOverview } from "@/components/SquadOverview";

export default function OverviewPage() {
  return <SquadOverview />;
}
