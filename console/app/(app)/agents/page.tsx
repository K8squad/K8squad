// app/agents/page.tsx — LEGACY Agents org-chart list, RETIRED by the ISI-5432 nav
// consolidation.
//
// The Agents surface folded into the unified Agents & Team two-axis page (/agents-team, epic
// ISI-5357/5362): the ORG axis' Teams ▸ Agents tree supersedes the session-resolved org chart
// and the admin fleet picker, with per-agent role chips and the roster filter. This route now
// only redirects so bookmarks, shared links and the owning-team deep links
// (/agents?team=<TeamUID> — from Projects, Skills and the old rail tree) land on the canonical
// surface with the admin cross-squad scoping param preserved. The /agents/{id} DETAIL route is
// NOT retired: it stays the drill-in with run history, reachable from the roster detail pane.
//
// Temporary (307) on purpose: 308s get cached hard by browsers and this console is still
// actively reorganizing — the redirect is a courtesy bridge, not a permanent contract.

import { redirect } from "next/navigation";

export const dynamic = "force-dynamic";

export default async function AgentsPage({
  searchParams,
}: {
  searchParams: Promise<{ team?: string }>;
}) {
  const { team } = await searchParams;
  redirect(
    team ? `/agents-team?team=${encodeURIComponent(team)}` : "/agents-team",
  );
}
