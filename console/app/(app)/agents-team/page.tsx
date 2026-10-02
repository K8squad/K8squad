// app/(app)/agents-team/page.tsx — the unified Agents & Team surface (ISI-5362 / S5, epic
// ISI-5357; mockups ISI-5306). The redesign folds the separate Agents / Teams / Skills nav nodes
// into ONE two-axis master-detail surface: ORG (Teams ▸ Agents) + LIBRARY (Roles, Skills) +
// ORG DEFAULTS (the ModelConfig singleton).
//
// Inline edit (Frame 02/03) is LIVE: the Model and Skills tabs write through the field-scoped
// merge PUT (ISI-5359, merged) with the role round-trip (ISI-5358, merged) — a save sends only
// its edited field, so unsent live fields survive. Shared-role edits confirm their blast radius
// (the usedBy index, ISI-5361) before committing. Full-form editing stays on /compose; nav
// consolidation lands with the follow-up increment.

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
          and the org default model. Open any node to read its detail; the Model and Skills tabs
          edit inline (shared-role changes show their blast radius first).
        </p>
      </header>
      <AgentsTeamWorkspace team={scopedTeam} />
    </main>
  );
}
