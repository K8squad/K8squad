"use client";
// components/KillRun.tsx — the FR-F4 ≤2-click kill (stories 3.3 + 8.4, ISI-2884).
//
// Click 1 arms (CONFIRM KILL), click 2 fires the POST through the BFF
// (/api/work-items/{id}/kill). The apiserver records the kill on the DURABLE
// machine (fence-first CancelEnter on the coord claim — Run → Canceling); the
// operator's drive loop + kill sweep tear the sandbox down and finish →
// Cancelled. The phase chips catch up via the overview/SSE projections, so
// this component optimistically marks the row killed on 200.
//
// Outcome rendering is honest: 409 (fence conflict) offers RETRY, 501 renders
// the "not hosted" legible state, other errors surface the status code. No
// fabricated success.

import { useState } from "react";

type KillState = "idle" | "armed" | "killing" | "done" | "conflict" | "error";

export function KillRun({ workItem, phase }: { workItem: string; phase?: string }) {
  const [state, setState] = useState<KillState>("idle");
  const [detail, setDetail] = useState<string>("");

  // Terminal Runs have nothing to kill; no work item = nothing to key the kill on.
  const terminal = phase === "Succeeded" || phase === "Failed" || phase === "Cancelled";
  if (!workItem || terminal) return null;

  async function fire() {
    setState("killing");
    try {
      const res = await fetch(`/api/work-items/${encodeURIComponent(workItem)}/kill`, {
        method: "POST",
      });
      if (res.status === 200) {
        setState("done");
        return;
      }
      if (res.status === 409) {
        setState("conflict");
        setDetail("a concurrent transition raced the kill");
        return;
      }
      if (res.status === 501) {
        setState("error");
        setDetail("This apiserver does not host the run-kill endpoint.");
        return;
      }
      setState("error");
      setDetail(`kill rejected (HTTP ${res.status})`);
    } catch {
      setState("error");
      setDetail("network error");
    }
  }

  if (state === "done") {
    return (
      <span className="phase-chip" data-tone="negative" data-testid="kill-issued">
        Kill issued → Canceling
      </span>
    );
  }

  return (
    <span style={{ display: "inline-flex", gap: 6, alignItems: "center" }}>
      {state === "armed" || state === "killing" ? (
        <>
          <button
            className="danger"
            data-testid="kill-confirm"
            disabled={state === "killing"}
            onClick={fire}
          >
            {state === "killing" ? "Killing…" : "Confirm kill"}
          </button>
          <button
            data-testid="kill-abort"
            disabled={state === "killing"}
            onClick={() => setState("idle")}
          >
            Keep run
          </button>
        </>
      ) : (
        <button
          data-testid="kill-arm"
          disabled={state === "error"}
          onClick={() => setState("armed")}
        >
          Kill
        </button>
      )}
      {state === "conflict" || state === "error" ? (
        <span className="muted" style={{ fontSize: 12 }} data-testid="kill-detail">
          {detail}.{" "}
          {state === "conflict" ? (
            <a
              href="#"
              onClick={(e) => {
                e.preventDefault();
                setState("idle");
              }}
            >
              Retry
            </a>
          ) : null}
        </span>
      ) : null}
    </span>
  );
}
