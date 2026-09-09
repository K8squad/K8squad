// app/projects/[projectId]/settings/page.tsx — Project → Settings tab mount (ISI-4000 / S2).
//
// The rail (ProjectsNavTree / lib/nav.ts PROJECT_SECTIONS) frames it; this server component decodes
// the route param (the project CR name) and mounts the client screen, which reads S1's projection
// through the BFF and composes the repo/PAT/test writes. Deep-linkable and active-highlighted from
// the pathname (AC8), exactly like every other project sub-route.

import { ProjectSettingsScreen } from "@/components/settings/ProjectSettingsScreen";

export default async function ProjectSettingsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return <ProjectSettingsScreen projectId={decodeURIComponent(projectId)} />;
}
