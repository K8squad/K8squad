// app/teams/page.tsx — LEGACY Teams list, RETIRED by the ISI-5432 nav consolidation.
//
// The Teams surface folded into the unified Agents & Team two-axis page (/agents-team, epic
// ISI-5357/5362): its ORG panel (Teams ▸ Agents, with counts, role chips and the filter box)
// supersedes this bare list, and the rail's Teams node is gone. This route now only redirects
// so bookmarks, shared links and the owning-team deep links (/teams?team=<TeamUID>) land on the
// canonical surface — the admin cross-squad scoping param is preserved verbatim (a tenant
// ignores it; /agents-team scopes to their one Team server-side). Temporary (307) on purpose:
// this console is still actively reorganizing; the redirect is a courtesy bridge, not a
// permanent contract.

import { redirect } from "next/navigation";

export const dynamic = "force-dynamic";

export default async function TeamsPage({
  searchParams,
}: {
  searchParams: Promise<{ team?: string }>;
}) {
  const { team } = await searchParams;
  redirect(
    team ? `/agents-team?team=${encodeURIComponent(team)}` : "/agents-team",
  );
}
