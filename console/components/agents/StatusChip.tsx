// components/agents/StatusChip.tsx — the shared agent-status chip (stories 8.10 + 8.11).
//
// Paints the four-value derived status (idle / running / blocked / paused) using the
// theme-invariant status hues (--status-*). When paused, it shows the §5.2 sub-reason
// ("paused: rate-limited" / "paused: credential", story 7.6). Pure presentational — no control.

import type { AgentStatus } from "@/lib/agents/types";
import { statusLabel } from "@/lib/agents/status";

export function StatusChip({
  status,
  pausedReason,
}: {
  status: AgentStatus;
  pausedReason?: "credential" | "rate_limited" | null;
}) {
  return (
    <span
      className={`agent-status agent-status--${status}`}
      data-status={status}
      title={statusLabel(status, pausedReason)}
    >
      <span className="agent-status__dot" aria-hidden="true" />
      {statusLabel(status, pausedReason)}
    </span>
  );
}
