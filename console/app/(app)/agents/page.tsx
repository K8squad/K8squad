// app/agents/page.tsx — the Agents nav node: the live squad org chart.
//
// The Agents section hosts the org diagram (Team → Agent → Role) + agent detail. The diagram is
// Team-scoped, and a session resolves to exactly ONE Team by UID (tenancy root — see SquadOverview),
// so there is no manual team selector: the landing auto-resolves the session's Team and renders its
// org diagram. An explicit `?team=<teamUID>` deep-link (e.g. from a shared URL) overrides that
// resolution. Read-only legibility: click an agent to open its detail and run history.

import Link from "next/link";

import { SessionTeamOrg } from "@/components/agents/SessionTeamOrg";
import { TeamOrgDiagram } from "@/components/agents/TeamOrgDiagram";
import { canCompose, viewer } from "@/lib/session";

export const dynamic = "force-dynamic";

export default async function AgentsPage({
  searchParams,
}: {
  searchParams: Promise<{ team?: string }>;
}) {
  const { team } = await searchParams;
  // "+ New Agent" is the discoverable entry point into the compose Agent form (ISI-3554 Story A).
  // Gated on a resolved session identity; the apiserver stays the write authority (see canCompose).
  const may = canCompose(await viewer());
  return (
    <main className="agents-page">
      <header className="agents-page__head">
        <div className="agents-page__head-text">
          <h1>Agents</h1>
          <p className="muted">
            Live org chart of your squad — Team → Agent → Role. Read-only: click
            an agent to open its detail and run history.
          </p>
        </div>
        {may ? (
          <Link
            className="btn btn--primary agents-page__new"
            href="/compose?kind=agents"
            aria-label="New Agent"
          >
            <span aria-hidden="true">+ </span>New Agent
          </Link>
        ) : (
          <button
            type="button"
            className="btn agents-page__new"
            disabled
            aria-disabled="true"
            aria-label="New Agent — sign in to add an agent"
            title="Sign in to add an agent"
          >
            <span aria-hidden="true">+ </span>New Agent
          </button>
        )}
      </header>
      <div className="card">
        {team ? <TeamOrgDiagram teamId={team} /> : <SessionTeamOrg />}
      </div>
    </main>
  );
}
