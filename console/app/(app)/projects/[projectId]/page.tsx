// app/projects/[projectId]/page.tsx — the project Overview surface, the workspace DEFAULT
// (ISI-3957 S1, AC2). Opening a project drops the operator into a control room, not a bare ticket
// list. The Overview lives at the bare project root (/projects/{id}) — the shareable "the project"
// URL — not an /overview sub-path.
//
// ISI-4508 (S3) replaces the old ProjectLanding panels WHOLESALE with the approved project-filtered
// dashboard (DESIGN-SPEC-ISI-4505 §3/§4, mock 02-project-overview-dashboard): a 5-up stat band,
// tickets-by-status-over-time + runs-by-status charts, live-thinking feed, and latest
// tickets/runs panels — composing the ISI-4506 S1 primitives and the ISI-4509 S4 read model, each
// panel degrading honestly on a slow/unrolled read.

import { ProjectOverviewDashboard } from "@/components/overview/ProjectOverviewDashboard";
import { decodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";

export default async function ProjectOverviewPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  // Decode the raw "ns%2Fname" segment once at the edge; the dashboard re-encodes exactly once
  // when it builds BFF URLs (ISI-3982).
  return (
    <main className="ksq-project-landing-page">
      <ProjectOverviewDashboard projectId={decodeProjectId(projectId)} />
    </main>
  );
}
