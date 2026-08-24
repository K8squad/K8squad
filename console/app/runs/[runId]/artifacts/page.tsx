// app/runs/[runId]/artifacts/page.tsx — the story 8.3 artifact inspection screen (ISI-2900).
//
// Mounts the ArtifactBrowser against the Run's coordination record through the BFF. Read-only
// inspection of artifact blobs + the structured handoff output; the build browser (8.7) stays a
// separate surface reachable from Run detail.

import { ArtifactBrowser } from "@/components/ArtifactBrowser";

export default async function RunArtifactsPage({
  params,
}: {
  params: Promise<{ runId: string }>;
}) {
  const { runId } = await params;
  return (
    <div>
      <header className="card">
        <h1 style={{ margin: 0 }}>Run {runId} — artifacts</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          Artifact blobs and handoff outputs from the coordination record (story 8.3).{" "}
          <a href={`/runs/${encodeURIComponent(runId)}`}>Back to the live stream</a>
        </p>
      </header>
      <ArtifactBrowser runId={runId} />
    </div>
  );
}
