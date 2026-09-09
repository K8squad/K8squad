// app/(app)/projects/[projectId]/settings/page.tsx — Project → Settings mount
// (ISI-4000 S2). The project sub-nav (layout.tsx / the ProjectsNavTree island)
// frames it; the client screen reads the S1 projection through the BFF and, for an
// authorized viewer, composes the existing repo/credential/repo-auth endpoints.

import { ProjectSettingsScreen } from "@/components/settings/ProjectSettingsScreen";

export default async function ProjectSettingsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return <ProjectSettingsScreen projectId={decodeURIComponent(projectId)} />;
}
