// app/projects/[projectId]/tickets/page.tsx — Project → Tickets mount (stories 8.13/8.14).
//
// The Tickets surface (Kanban 8.14b / List 8.14c over the 8.17 sub-ticket tree) lands with
// ISI-2910; the project-scoped route + sub-nav tab exist now (story 8.13 IA).

export default async function TicketsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return (
    <div>
      <h1>Tickets</h1>
      <p className="muted">
        Kanban + List views for{" "}
        <strong>{decodeURIComponent(projectId)}</strong> (stories 8.14/8.17 — in
        flight under [ISI-2910](/ISI/issues/ISI-2910)). Status transitions go
        through the 8.14a endpoint; this screen never writes client-authored
        state.
      </p>
    </div>
  );
}
