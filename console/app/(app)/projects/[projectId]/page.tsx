// app/projects/[projectId]/page.tsx — the Landing surface, the workspace DEFAULT (ISI-3957 S1,
// AC2). The old forced redirect to /tickets is gone: opening a project drops the operator into a
// control room, not a bare ticket list. Landing lives at the bare project root (/projects/{id}) —
// the shareable "the project" URL — not a /landing sub-path.
//
// S1 ships the frame; S2 (ISI-3958, ProjectLanding) supplies the three read-only panels
// (Runs · Stats · Latest Tickets) with their own honest loading/empty/error/not-available states.

import { ProjectLanding } from "@/components/projects/ProjectLanding";
import { decodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";

export default async function ProjectLandingPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  // Decode the raw "ns%2Fname" segment once at the edge; ProjectLanding re-encodes exactly once
  // when it builds BFF URLs (ISI-3982).
  return (
    <main className="ksq-project-landing-page">
      <ProjectLanding projectId={decodeProjectId(projectId)} />
    </main>
  );
}
