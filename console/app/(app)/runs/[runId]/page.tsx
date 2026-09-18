// app/runs/[runId]/page.tsx — Run detail screen (ISI-4576): phase header, steps +
// thinking/comments timeline, artifacts and the live SSE timeline (story 8.2),
// with the FR-F4 Kill Run control (stories 3.3 + 8.4, ISI-2884).
//
// RunDetail is a client component: it fetches the ISI-4571 detail read model through the
// BFF and opens the ONE EventSource via RunStream — the browser never talks to the
// apiserver directly (arch §13 / ADR-013). Kill is a control-plane POST, not a stream
// verb, so the stream stays read-only (AC6).

import { RunDetail } from "@/components/RunDetail";

export default async function RunPage({
  params,
}: {
  params: Promise<{ runId: string }>;
}) {
  const { runId } = await params;
  return <RunDetail runId={runId} />;
}
