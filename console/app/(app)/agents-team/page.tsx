// app/(app)/agents-team/page.tsx — the unified Agents & Team surface (ISI-5362 / S5, epic
// ISI-5357; mockups ISI-5306). The redesign folds the separate Agents / Teams / Skills nav nodes
// into ONE two-axis master-detail surface: ORG (Teams ▸ Agents) + LIBRARY (Roles, Skills) +
// ORG DEFAULTS (the ModelConfig singleton).
//
// This increment is the READ-ONLY surface (rail + detail + effective-model provenance). Inline
// edit-save (Frame 02) is GATED on ISI-5358 (role round-trip) + ISI-5359 (field-scoped merge
// writes) — until those land on main, a save could silently drop unsent fields — so every mutate
// affordance renders disabled (see NodeDetail). The surface ships as its own route first so it does
// not regress the shipped /agents page while the redesign is completed; nav consolidation lands
// with the edit path.

import { AgentsTeamWorkspace } from "@/components/agents/roster/AgentsTeamWorkspace";
import { viewer } from "@/lib/session";

export const dynamic = "force-dynamic";

export default async function AgentsTeamPage({
  searchParams,
}: {
  searchParams: Promise<{ team?: string }>;
}) {
  const { team } = await searchParams;
  // A global admin has no home tenancy; the ?team= selector scopes an explicit cross-squad read
  // (the same act-as-team seam the squad LIST reads already honour). A tenant omits it and the
  // server scopes to their one Team.
  const v = await viewer();
  const scopedTeam = v.access === "admin" ? team : undefined;

  return (
    <main className="agents-team-page">
      <header className="agents-team-page__head">
        <h1>Agents &amp; Team</h1>
        <p className="muted">
          Your org, roles and skills in one place — Teams ▸ Agents, the shared Roles/Skills library,
          and the org default model. Read-only: click any node to see its detail and effective model.
        </p>
      </header>
      <AgentsTeamWorkspace team={scopedTeam} />
    </main>
  );
}
