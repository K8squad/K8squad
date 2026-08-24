// app/runs/[runId]/page.tsx — Run detail with the live SSE timeline (story 8.2).
//
// The RunStream client component opens the ONE EventSource against the BFF (never the apiserver).
// The Run-detail header is where the separate FR-F4 Kill Run control (story 3.3/8.4) mounts — it
// is a control-plane POST, NOT a stream verb, so the stream itself stays read-only (AC6).

import { RunStream } from "@/components/RunStream";

export default async function RunPage({
  params,
}: {
  params: Promise<{ runId: string }>;
}) {
  const { runId } = await params;
  return (
    <div>
      <header className="card">
        <h1 style={{ margin: 0 }}>Run {runId}</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          Live coordination progress. Kill Run (FR-F4) is a separate authorized
          control-plane action, not a stream verb.{" "}
          <a href={`/runs/${encodeURIComponent(runId)}/artifacts`}>
            Inspect artifacts &amp; handoff (8.3)
          </a>
        </p>
      </header>
      <div className="card">
        <RunStream runId={runId} />
      </div>
    </div>
  );
}
