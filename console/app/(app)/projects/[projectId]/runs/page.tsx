// app/projects/[projectId]/runs/page.tsx — the project-scoped runs list (ISI-4575), replacing
// the stories 8.2/8.13 placeholder now that the ISI-4571 project run-list read model exists.
// Run DETAIL (live SSE stream) stays at the global /runs/[runId] route — rows deep-link there.

import { RunsList } from "@/components/runs/RunsList";
import { decodeProjectId } from "@/lib/projectId";

export default async function ProjectRunsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  const name = decodeProjectId(projectId);
  return <RunsList projectId={name} projectName={name} />;
}
