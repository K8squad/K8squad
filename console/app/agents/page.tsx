// app/agents/page.tsx — Agents (org diagram + agent detail) mount point (stories 8.10/8.11).
//
// GLOBAL surface, filterable by squad/project once the org read model lands (ISI-2907).
// Route exists now so the rail node resolves and the IA hierarchy is navigable.

export default function AgentsPage() {
  return (
    <div>
      <h1>Agents</h1>
      <p className="muted">
        Team→Agent→Role organization diagram (8.10) and agent detail with Run
        history + logs (8.11) — in flight under
        [ISI-2907](/ISI/issues/ISI-2907).
      </p>
    </div>
  );
}
