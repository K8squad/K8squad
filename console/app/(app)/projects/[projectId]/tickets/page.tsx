// Project-scoped Tickets route (stories 8.14b–d + 8.17). Mounted inside the
// Epic 8 shell (app/layout.tsx) at Project → Tickets per the Project-rooted nav
// IA (8.13). Thin server component: all interactive work (dual view, filters,
// tree, DnD) lives in the client TicketsScreen; reads and the one mutation ride
// the BFF choke point (ADR-013).

import { TicketsScreen } from "@/components/tickets/TicketsScreen";

export const dynamic = "force-dynamic";

export default async function TicketsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return (
    <main className="ksq-tickets-page">
      <TicketsScreen projectId={projectId} />
    </main>
  );
}
