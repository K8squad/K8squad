// lib/runs.ts — wire types + fetch for the run detail read model
// (ISI-4571 backend: internal/apiserver/runs.go RunDetailResponse; ISI-4576 screen).
//
// The browser fetches the BFF proxy (`/api/runs/{runId}`), never the Go apiserver
// directly (arch §13 / ADR-013). The response embeds the full Run CRD object plus
// coord-derived steps, progressmirror thinking/comments and LLM interaction digests.

export interface RunStepWire {
  id: string;
  name: string;
  description?: string;
  startedAt?: string | null;
  completedAt?: string | null;
  status: string;
  error?: string | null;
}

export interface ThinkingEntryWire {
  id: string;
  type: "comment" | "tool_use" | "observation" | "llm_interaction" | string;
  content: string;
  agent?: string;
  timestamp: string;
}

export interface LLMInteractionWire {
  id: string;
  model: string;
  role: string;
  content: string;
  timestamp: string;
  tokensUsed?: number;
}

/** The subset of the Run CRD the detail screen reads (api/v1alpha1/run_types.go). */
export interface RunWire {
  metadata?: { name?: string; namespace?: string; creationTimestamp?: string };
  spec: {
    projectRef?: { name?: string };
    workItemRef?: string;
    agents?: { name?: string }[];
  };
  status?: {
    phase?: string;
    claimedAt?: string;
    traceID?: string;
    totalTokenUsage?: { totalTokens?: number; inputTokens?: number; outputTokens?: number };
    artifactRefs?: { name?: string }[];
    conditions?: { type: string; reason?: string }[];
  };
}

/** One sub-ticket a run authored via work_item_create (ADR-0024a S6, ISI-4872),
 * sourced from the run-scoped `work_item_created` audit rows — ground truth, not
 * the agent's completion prose. */
export interface CreatedWorkItemWire {
  id: string;
  title: string;
}

export interface RunDetailResponseWire {
  run: RunWire;
  steps?: RunStepWire[] | null;
  thinking?: ThinkingEntryWire[] | null;
  llmInteractions?: LLMInteractionWire[] | null;
  /** Sub-tickets this run created on the board (ISI-4872). Absent/empty when the
   * run authored none — the screen renders the honest zero, never a fabrication. */
  createdItems?: CreatedWorkItemWire[] | null;
}

export type RunDetailState =
  | { kind: "loading" }
  | { kind: "error"; status: number }
  | { kind: "ready"; detail: RunDetailResponseWire };

export async function fetchRunDetail(
  runId: string,
  signal?: AbortSignal,
): Promise<RunDetailState> {
  try {
    const res = await fetch(`/api/runs/${encodeURIComponent(runId)}`, {
      headers: { accept: "application/json" },
      signal,
    });
    if (!res.ok) return { kind: "error", status: res.status };
    return { kind: "ready", detail: (await res.json()) as RunDetailResponseWire };
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") {
      return { kind: "loading" };
    }
    return { kind: "error", status: 0 };
  }
}
