// app/agents/page.tsx — the Agents nav node: the live squad org chart.
//
// The Agents section hosts the org diagram (Team → Agent → Role) + agent detail. The diagram is
// Team-scoped, and a session resolves to exactly ONE Team by UID (tenancy root — see SquadOverview),
// so there is no manual team selector: the landing auto-resolves the session's Team and renders its
// org diagram. An explicit `?team=<teamUID>` deep-link (e.g. from a shared URL) overrides that
// resolution. Read-only legibility: click an agent to open its detail and run history.

import { SessionTeamOrg } from "@/components/agents/SessionTeamOrg";
import { TeamOrgDiagram } from "@/components/agents/TeamOrgDiagram";

export const dynamic = "force-dynamic";

export default async function AgentsPage({
  searchParams,
}: {
  searchParams: Promise<{ team?: string }>;
}) {
  const { team } = await searchParams;
  return (
    <main className="agents-page">
      <header className="agents-page__head">
        <h1>Agents</h1>
        <p className="muted">
          Live org chart of your squad — Team → Agent → Role. Read-only: click
          an agent to open its detail and run history.
        </p>
      </header>
      <div className="card">
        {team ? <TeamOrgDiagram teamId={team} /> : <SessionTeamOrg />}
      </div>
    </main>
  );
}
