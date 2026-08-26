// app/projects/[projectId]/page.tsx — Project root redirect (story 8.13).
//
// The Project node expands to its sub-nav; Tickets is the default project surface, so the
// project root lands there (UX screen 14 — Project → Tickets).

import { redirect } from "next/navigation";

export default async function ProjectRootPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  redirect(`/projects/${encodeURIComponent(projectId)}/tickets`);
}
