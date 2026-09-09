// app/projects/[projectId]/issues/page.tsx — Project → Issues mount (ISI-3957 S1).
//
// "Issues" is the Project-Detail Workspace's vocabulary for the project work-item list (the board's
// and S2's term; S2's Landing "view all tickets" links here). The live list surface is the same
// client TicketsScreen the legacy /tickets route mounts — reused, not duplicated — so the one
// screen + its BFF reads/mutation (ADR-013) back both URLs. S3 (ISI-3959) owns the create/modify
// writes that land inside this screen; S1 only guarantees the Issues destination resolves.

import { TicketsScreen } from "@/components/tickets/TicketsScreen";
import { decodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";

export default async function IssuesPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  // Decode the raw "ns%2Fname" segment once at the edge; the screen re-encodes exactly once
  // when it builds BFF URLs (ISI-3982).
  return (
    <main className="ksq-tickets-page">
      <TicketsScreen projectId={decodeProjectId(projectId)} />
    </main>
  );
}
