// app/projects/[projectId]/page.tsx — Project root redirect (story 8.13).
//
// The Project node expands to its sub-nav; Tickets is the default project surface, so the
// project root lands there (UX screen 14 — Project → Tickets).

import { redirect } from "next/navigation";
import { encodeProjectId } from "@/lib/projectId";

export default async function ProjectRootPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  // Next.js hands us the still-encoded "ns%2Fname" segment; re-encoding it here
  // double-encoded the redirect target ("ns%252Fname") and 404'd downstream
  // (ISI-3982). Normalize to exactly one layer of encoding.
  redirect(`/projects/${encodeProjectId(projectId)}/tickets`);
}
