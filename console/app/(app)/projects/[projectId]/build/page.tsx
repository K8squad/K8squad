// app/projects/[projectId]/build/page.tsx — Project → Build mount (stories 8.7/8.13).
//
// The three-pane build browser (8.7e) lands with ISI-2904; this project-scoped mount keeps
// the Project sub-nav complete (Build · Tickets · Runs · Discussion, story 8.13).

export default async function BuildPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  return (
    <div>
      <h1>Build</h1>
      <p className="muted">
        Build snapshots for <strong>{decodeURIComponent(projectId)}</strong>.
        Open a Run to inspect its build workspace — the per-Run file tree, diffs,
        and code views.
      </p>
    </div>
  );
}
