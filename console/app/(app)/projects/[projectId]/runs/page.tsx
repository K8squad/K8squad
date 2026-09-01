// app/projects/[projectId]/runs/page.tsx — Project → Runs mount (stories 8.2/8.13).
//
// Project-scoped Run list. Run DETAIL (live SSE stream) stays at the existing global route
// /runs/[runId] (story 8.2) — re-parented unchanged per the 8.13 scope guard; only this
// project-scoped listing route is new. The Run list read model arrives with the Run-history
// work under ISI-2907/ISI-2904.

export default async function RunsPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return (
    <div>
      <h1>Runs</h1>
      <p className="muted">
        Run history for{" "}
        <strong>{decodeURIComponent(projectId)}</strong>. Open a run at{" "}
        <code>/runs/&lt;runId&gt;</code> to watch its coordination events stream
        live through the BFF.
      </p>
    </div>
  );
}
