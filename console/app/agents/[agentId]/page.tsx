// app/agents/[agentId]/page.tsx — agent detail with Run history + logs (story 8.11).
//
// Reached from the org diagram (8.10, node deep-link) or overview (8.1). The thin App Router entry
// inside the Epic 8 shell; the detail view is a client component (live SSE log tail on active Runs).
// Read/legibility surface (R6): Run history + tabbed logs + OTel-trace deep-links, no coordination
// affordance. Deny is existence-hiding — a foreign / missing Agent renders identically (404-not-403).

import { AgentDetail } from "@/components/agents/AgentDetail";

export const dynamic = "force-dynamic";

export default async function AgentDetailPage({
  params,
}: {
  params: Promise<{ agentId: string }>;
}) {
  const { agentId } = await params;
  return (
    <main className="agent-detail-page">
      <AgentDetail agentId={agentId} />
    </main>
  );
}
