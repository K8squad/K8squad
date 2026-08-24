// app/overview/page.tsx — Overview (squad status/activity) mount point (story 8.13).
//
// GLOBAL surface (8.1 squad overview). The live read-model wiring lands with ISI-2900; the
// nav shell mounts the route now so the hierarchy exists and the rail node resolves.

export default function OverviewPage() {
  return (
    <div>
      <h1>Overview</h1>
      <p className="muted">
        Squad status and activity (story 8.1 — wiring in flight under
        [ISI-2900](/ISI/issues/ISI-2900)). The apiserver read model this screen
        consumes is <code>GET /api/squad/overview</code>.
      </p>
    </div>
  );
}
