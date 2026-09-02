// app/agents/page.tsx — the Agents nav node (stories 8.10 + 8.13 IA).
//
// The console's "Agents" section (nav IA, CEO 2026-08-12 / story 8.13) hosts the org diagram (8.10)
// + agent detail (8.11). The org diagram is Team-scoped (8.10 "Given a Team…"), so this landing
// renders it for the selected Team via `?team=<teamId>`. Read/legibility surface (R6): no
// compose/edit (that stays 8.5) and no coordination affordance. The diagram itself is a client
// component (live SSE status); this route is the thin App Router entry inside the Epic 8 shell.

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
          Live squad org chart (Team → Agent → Role). Read-only legibility —
          composition stays in the compose view, coordination stays
          server-side. Click an agent to open its detail + run history.
        </p>
      </header>
      {team ? (
        <div className="card">
          <TeamOrgDiagram teamId={team} />
        </div>
      ) : (
        <div className="card">
          <p className="muted">
            Select a team to view its org diagram (e.g.{" "}
            <code>/agents?team=&lt;teamId&gt;</code>).
          </p>
        </div>
      )}
    </main>
  );
}
