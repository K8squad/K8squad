// app/projects/[projectId]/issues/[workItemId]/page.tsx — the ticket-detail
// mount (ISI-4399 S3, design ISI-4231 §3). Nested under the Issues route so it
// inherits the Project workspace shell (rail + breadcrumb); the client
// TicketDetail draws the header, description, sub-tickets, and the agent Activity
// thread from the reads the console already owns (no new backend).

import { TicketDetail } from "@/components/tickets/TicketDetail";
import { decodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";

export default async function TicketDetailPage({
  params,
}: {
  params: Promise<{ projectId: string; workItemId: string }>;
}) {
  const { projectId, workItemId } = await params;
  // Decode the raw "ns%2Fname" segment once at the edge; the client re-encodes
  // exactly once when it builds BFF URLs (ISI-3982).
  return (
    <main className="ksq-tickets-page">
      <TicketDetail
        projectId={decodeProjectId(projectId)}
        workItemId={decodeURIComponent(workItemId)}
      />
    </main>
  );
}
