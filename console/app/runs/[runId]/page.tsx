// app/runs/[runId]/page.tsx — Run detail with the live SSE timeline (story 8.2)
// and the FR-F4 Kill Run control (stories 3.3 + 8.4, ISI-2884).
//
// The RunStream client component opens the ONE EventSource against the BFF (never the apiserver).
// The Kill control is a control-plane POST, NOT a stream verb, so the stream itself stays
// read-only (AC6). Its work-item key rides the overview's run link (?wi=…) — the coord claim
// key, the value every read model already carries.

import { KillRun } from "@/components/KillRun";
import { RunStream } from "@/components/RunStream";

export default async function RunPage({
  params,
  searchParams,
}: {
  params: Promise<{ runId: string }>;
  searchParams: Promise<{ wi?: string; phase?: string }>;
}) {
  const { runId } = await params;
  const { wi, phase } = await searchParams;
  return (
    <div>
      <header className="card">
        <h1 style={{ margin: 0 }}>Run {runId}</h1>
        <p className="muted" style={{ margin: "6px 0 0" }}>
          Live coordination progress.{" "}
          <a href={`/runs/${encodeURIComponent(runId)}/artifacts`}>
            Inspect artifacts &amp; handoff (8.3)
          </a>
        </p>
        {wi ? (
          <div style={{ marginTop: 8 }}>
            <KillRun workItem={wi} phase={phase} />
          </div>
        ) : null}
      </header>
      <div className="card">
        <RunStream runId={runId} />
      </div>
    </div>
  );
}
