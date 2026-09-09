// app/projects/[projectId]/files/page.tsx — Project → File Explorer mount
// (ISI-3956 S4c). The project sub-nav (layout.tsx) frames it; the client tab
// fetches the S4b read model through the BFF and renders the read-only tree +
// preview with honest loading/busy/empty/501 states.

import { FileExplorerTab } from "@/components/FileExplorerTab";

export default async function FilesPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return <FileExplorerTab projectId={decodeURIComponent(projectId)} />;
}
