// app/runs/[runId]/build/page.tsx — the story 8.7e three-pane build-browser screen (ISI-2904).
//
// Mounts the BuildBrowser against the Run's git read-model through the BFF. Read-only inspection of
// the Run's build workspace — file tree, per-file bytes, and the unified diff against the base ref —
// behind the apiserver's 8.7d per-principal + Team-scope gate (existence-hiding 404). Separate from
// the 8.3 artifact browser: this is the SOURCE (git plumbing), that is the coordination record.

import { BuildBrowser } from "@/components/BuildBrowser";

export default async function RunBuildPage({
  params,
}: {
  params: Promise<{ runId: string }>;
}) {
  const { runId } = await params;
  return (
    <div>
      <header className="card">
        <h1 style={{ margin: 0 }}>Run {runId} — build</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          The Run&apos;s build workspace: file tree, per-file bytes, and the diff against its base
          ref (story 8.7).{" "}
          <a href={`/runs/${encodeURIComponent(runId)}`}>Back to the live stream</a>
        </p>
      </header>
      <BuildBrowser runId={runId} />
    </div>
  );
}
