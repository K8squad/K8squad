"use client";

// components/tickets/TicketProposals.tsx — ISI-5284 (WS-4): the coordinator
// propose-mode proposal surface, ported to the TICKET view.
//
// Parent: ISI-5267 §3.5. A Team's Coordinator role dispatched in PROPOSE mode
// (capability coordinator.propose, ISI-5282 / WS-3) does NOT author directly: each
// work_item_create / work_item_assign RAISES an inert kind='proposal' card a human
// confirms, and the confirm fans into the EXISTING proposalconfirm.go path. WS-3
// already wires this for the discussion ROOM; this component renders the SAME cards
// on the ticket surface, REUSING:
//   - ProposalCard verbatim (components/discussion/ProposalCard.tsx);
//   - the lib/discussion/api.ts client (listProposals / confirmProposal /
//     dismissProposal) + its confirm/dismiss BFF routes;
//   - the ISI-5174/5196 working-indicator pill for the "manager is structuring the
//     work" affordance (TicketWorkingIndicator + ticketManagerStructuringView).
//
// ─────────────────────────────────────────────────────────────────────────────
// THREAD-MAPPING GAP (honest Option B — see ISI-5284 report + §"open question"):
// Proposals are DISCUSSION-thread-scoped: the coordinator posts them back into the
// room thread its run was dispatched FROM, resolved server-side from the run id via
// the mention-dispatch ledger (internal/mentiondispatch ProposalThreadResolver →
// coord.claim.run_id → mention_dispatch.thread_id). That ledger row exists ONLY for
// a ROOM @-mention dispatch. The ticket surface is keyed on a workItemId whose
// WorkItemThread is a BOARD detail (comments/history), NOT a discussion thread — and
// a coordinator dispatched FROM a ticket (ISI-5283 WS-2 routes it through the board
// comment-&-assign verb) gets NO mention_dispatch row, so no reachable discussion
// threadId exists in merged main, and no HTTP route exposes ThreadForDispatchedRun.
//
// So this component is PARAMETERIZED on `threadId`: given a reachable discussion
// thread it renders + confirms the live cards; absent one it renders the honest D3
// fallback (NEVER fabricated proposals). Wiring a ticket-originated coordinator to a
// reachable proposal thread (either routing the ticket multi-mention through the
// room mention path, or exposing a run→thread lookup over the ledger) is owned by
// ISI-5283 (WS-2, in_review) / a WS-2 follow-up — tracked as the backend gap.
// ─────────────────────────────────────────────────────────────────────────────

import { useCallback, useEffect, useState } from "react";
import { ProposalCard } from "@/components/discussion/ProposalCard";
import { TicketWorkingIndicator } from "./TicketWorkingIndicator";
import { ticketManagerStructuringView } from "@/lib/tickets/working";
import type { DispatchState } from "@/lib/tickets/useDispatchWatch";
import { createDiscussionClient, type DiscussionClient } from "@/lib/discussion/api";
import type { Proposal } from "@/lib/discussion/types";

/** The in-flight coordinator orchestration run the "structuring the work" pill tracks. */
export interface StructuringManager {
  /** Coordinator role/agent name shown in the pill. */
  name: string;
  /** The ticket's honest run ladder for that coordinator run (ISI-4853). */
  state: DispatchState;
}

export interface TicketProposalsProps {
  projectId: string;
  /**
   * The discussion thread the coordinator posts its proposals into — null when no
   * reachable thread is linked to this ticket (the honest default on merged main;
   * see the thread-mapping note above). Null ⇒ the D3 fallback, never a fake card.
   */
  threadId: string | null;
  /**
   * False ⇒ this project/team has NO coordinator role, so the D3 fallback tells the
   * human to assign agents individually rather than implying orchestration is
   * available. Undefined ⇒ coordinator presence unknown (roster not loaded / not
   * carried by this deployment) — the fallback stays neutral.
   */
  hasCoordinator?: boolean;
  /** The live coordinator orchestration run, or null when none is in flight. */
  manager?: StructuringManager | null;
  /** Re-sync the sub-ticket tree after a confirm executes (a new child ticket lands). */
  onExecuted?: () => void;
  /** Deep-link to the project's issues list for the D3 "assign individually" prompt. */
  issuesHref: string;
  /** Test seam: inject a stub client. Defaults to the real BFF-backed client. */
  client?: DiscussionClient;
}

type LoadState = "idle" | "loading" | "ready" | "error";

export function TicketProposals({
  projectId,
  threadId,
  hasCoordinator,
  manager,
  onExecuted,
  issuesHref,
  client,
}: TicketProposalsProps) {
  // One stable client for the lifetime of the mount (createDiscussionClient is a
  // thin wrapper over fetch; recreating it per render is harmless but pointless).
  const [api] = useState<DiscussionClient>(() => client ?? createDiscussionClient());
  const [proposals, setProposals] = useState<Record<string, Proposal>>({});
  const [busyMessageId, setBusyMessageId] = useState<string | undefined>();
  const [load, setLoad] = useState<LoadState>(threadId ? "loading" : "idle");

  useEffect(() => {
    if (!threadId) {
      setProposals({});
      setLoad("idle");
      return;
    }
    let alive = true;
    setLoad("loading");
    api
      .listProposals(projectId, threadId)
      .then((list) => {
        if (!alive) return;
        const next: Record<string, Proposal> = {};
        for (const p of list) next[p.Message.id] = p;
        setProposals(next);
        setLoad("ready");
      })
      .catch(() => {
        // A deny/absent read is "no proposals here" — never leak, never fabricate.
        if (!alive) return;
        setProposals({});
        setLoad("error");
      });
    return () => {
      alive = false;
    };
  }, [api, projectId, threadId]);

  // Human confirm (plan §4.4): fan into the proposalconfirm.go seam, advance the
  // card phase from the server's verdict, and — once a create executes — reload the
  // ticket's children so the new sub-ticket surfaces in the tree (no fabrication).
  const confirmProposal = useCallback(
    async (messageId: string) => {
      setBusyMessageId(messageId);
      try {
        const res = await api.confirmProposal(projectId, messageId);
        setProposals((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: res.status } };
        });
        if (res.status === "executed" || res.status === "confirmed") onExecuted?.();
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [api, projectId, onExecuted],
  );

  // Human dismiss (plan §4.4): record the decision, no fan-out.
  const dismissProposal = useCallback(
    async (messageId: string) => {
      setBusyMessageId(messageId);
      try {
        await api.dismissProposal(projectId, messageId);
        setProposals((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: "dismissed" } };
        });
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [api, projectId],
  );

  const cards = Object.values(proposals);
  const structuring = manager
    ? ticketManagerStructuringView(manager.name, manager.state)
    : null;
  // The honest D3 / empty state: no reachable thread, or a reachable thread that
  // carries no proposal cards. Never rendered alongside live cards.
  const showFallback = cards.length === 0 && load !== "loading";

  return (
    <section className="card ksq-ticket-proposals" data-testid="detail-proposals">
      <h2>Coordinator proposals</h2>

      {/* AC4 — "Manager is structuring the work" pill, reusing the ISI-5174/5196
          working-indicator markup + a11y; shown while the coordinator's propose
          run is live, resolving to structured / couldn’t-structure on terminal. */}
      {structuring && (
        <div className="ksq-ticket-proposals__working" data-testid="detail-proposals-working">
          <TicketWorkingIndicator view={structuring} />
        </div>
      )}

      {load === "loading" && (
        <p className="muted" data-testid="detail-proposals-loading">
          Loading proposals…
        </p>
      )}

      {cards.length > 0 && (
        <ul className="ksq-ticket-proposals__list" data-testid="detail-proposals-list">
          {cards.map((p) => (
            <li key={p.Message.id}>
              <ProposalCard
                proposal={p}
                onConfirm={confirmProposal}
                onDismiss={dismissProposal}
                busy={busyMessageId === p.Message.id}
              />
            </li>
          ))}
        </ul>
      )}

      {showFallback && (
        <ProposalsFallback hasCoordinator={hasCoordinator} issuesHref={issuesHref} />
      )}
    </section>
  );
}

/**
 * D3 fallback (AC3): a lightweight prompt, NEVER a silent fan-out. Three honest
 * shapes, by what we actually know:
 *   - hasCoordinator === false → the project has no coordinator role: tell the human
 *     to assign agents individually (via the composer / issues list).
 *   - threadId null but coordinator may exist → no proposals are linked to this
 *     ticket yet (the merged-main thread-mapping gap); say so plainly.
 *   - a reachable thread with zero cards → simply "no proposals yet".
 */
function ProposalsFallback({
  hasCoordinator,
  issuesHref,
}: {
  hasCoordinator?: boolean;
  issuesHref: string;
}) {
  if (hasCoordinator === false) {
    return (
      <div className="ksq-empty-hint" data-testid="detail-proposals-no-coordinator">
        <p className="muted">
          No coordinator is configured for this project, so mentioning several agents
          won’t structure the work automatically. Assign agents individually with
          “Comment &amp; assign” below.
        </p>
        <a href={issuesHref} className="ksq-link" data-testid="detail-proposals-assign-link">
          Open the issues board
        </a>
      </div>
    );
  }
  return (
    <p className="muted" data-testid="detail-proposals-empty">
      No coordinator proposals for this ticket yet. When a coordinator structures the
      work it mentions here, the proposed sub-tickets will appear above for you to
      confirm.
    </p>
  );
}

export default TicketProposals;
