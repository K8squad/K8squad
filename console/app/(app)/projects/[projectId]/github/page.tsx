// app/projects/[projectId]/github/page.tsx — Project → GitHub status mount
// (ISI-3956 S5c). The project sub-nav (layout.tsx) frames it; the client tab
// fetches the S5b read model through the BFF and renders the mirror projection
// with honest freshness.

import { GitHubStatusTab } from "@/components/GitHubStatusTab";

export default async function GithubStatusPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return <GitHubStatusTab projectId={decodeURIComponent(projectId)} />;
}
