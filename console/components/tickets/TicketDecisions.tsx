"use client";

// components/tickets/TicketDecisions.tsx — ISI-5536 (ISI-5531 E2, ADR-0026 §4/§7):
// the decision_request surface on the TICKET detail page.
//
// Net-new "an agent suggests structured options a human answers" (approve / choose_one /
// choose_many / free_form). This is the FE-4 container: the sibling of TicketProposals,
// rendering the SAME DecisionCard the Inbox row will (components/inbox/DecisionCard.tsx)
// and driving it through the lib/discussion/api.ts client
// (listDecisionRequests / answerDecisionRequest / rejectDecisionRequest) + its BFF routes.
//
// ─────────────────────────────────────────────────────────────────────────────
// THREAD-MAPPING: decision_request cards are DISCUSSION-thread-scoped exactly like
// proposals — an agent posts them into the room thread its run is dispatched from.
// So this component is PARAMETERIZED on the SAME `threadId` seam TicketProposals uses
// (TicketDetail's proposalThreadId): given a reachable discussion thread it renders +
// answers the live cards; absent one (the merged-main thread-mapping gap, owned by
// ISI-5283 WS-2 / follow-up) it renders nothing — NEVER a fabricated card.
// ─────────────────────────────────────────────────────────────────────────────

import { useCallback, useEffect, useState } from "react";
import { DecisionCard, type DecisionAnswerInput } from "@/components/inbox/DecisionCard";
import { createDiscussionClient, type DiscussionClient } from "@/lib/discussion/api";
import type { DecisionRequest } from "@/lib/discussion/types";

export interface TicketDecisionsProps {
  projectId: string;
  /**
   * The discussion thread the decision cards live in — null when no reachable thread
   * is linked to this ticket (the honest default on merged main; same seam as
   * TicketProposals). Null ⇒ render nothing, never a fake card.
   */
  threadId: string | null;
  /**
   * Re-sync the ticket after a decision resolves — answering re-dispatches the raising
   * agent (ADR-0026 §5), so the ticket's run ladder / children may advance.
   */
  onDecided?: () => void;
  /** Test seam: inject a stub client. Defaults to the real BFF-backed client. */
  client?: DiscussionClient;
}

/**
 * Map an answer/reject failure to a human-readable, non-leaking note. `DiscussionApiError`
 * carries the HTTP status; a denied/missing read already collapses to 404 at the client, so
 * these messages never reveal a foreign room — they only tell the human why their click didn't
 * take and what to do next.
 */
function decisionErrorMessage(e: unknown): string {
  const status = (e as { status?: number } | null)?.status;
  switch (status) {
    case 409:
      return "This decision was already answered. Refresh to see the latest.";
    case 400:
      return "That answer wasn't accepted — check your selection and try again.";
    case 401:
    case 403:
    case 404:
      return "You can no longer act on this decision.";
    default:
      return "Something went wrong submitting your answer. Please try again.";
  }
}

export function TicketDecisions({
  projectId,
  threadId,
  onDecided,
  client,
}: TicketDecisionsProps) {
  const [api] = useState<DiscussionClient>(() => client ?? createDiscussionClient());
  const [decisions, setDecisions] = useState<Record<string, DecisionRequest>>({});
  const [busyMessageId, setBusyMessageId] = useState<string | undefined>();
  const [errorByMessageId, setErrorByMessageId] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!threadId) {
      setDecisions({});
      return;
    }
    let alive = true;
    api
      .listDecisionRequests(projectId, threadId)
      .then((list) => {
        if (!alive) return;
        const next: Record<string, DecisionRequest> = {};
        for (const d of list) next[d.Message.id] = d;
        setDecisions(next);
      })
      .catch(() => {
        // A deny/absent read is "no decisions here" — never leak, never fabricate.
        if (!alive) return;
        setDecisions({});
      });
    return () => {
      alive = false;
    };
  }, [api, projectId, threadId]);

  const clearError = useCallback((messageId: string) => {
    setErrorByMessageId((cur) => {
      if (!(messageId in cur)) return cur;
      const next = { ...cur };
      delete next[messageId];
      return next;
    });
  }, []);

  // Human answer (ADR-0026 §4.4): the apiserver derives the mode from the stored card,
  // validates the selection, records the post-back, and re-dispatches the raising agent.
  // Advance the card to its returned phase so it renders read-only without a reload.
  const answerDecision = useCallback(
    async (messageId: string, answer: DecisionAnswerInput) => {
      setBusyMessageId(messageId);
      clearError(messageId);
      try {
        const res = await api.answerDecisionRequest(projectId, messageId, answer);
        setDecisions((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: res.status } };
        });
        onDecided?.();
      } catch (e) {
        // A 4xx/409 must surface, never leave a silently stuck card: keep the card `open`
        // (no phase advance, no onDecided) and show why the answer didn't land.
        setErrorByMessageId((cur) => ({ ...cur, [messageId]: decisionErrorMessage(e) }));
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [api, projectId, onDecided, clearError],
  );

  // Human reject (ADR-0026 §4.4): records the rejection + reason and re-dispatches the
  // raising agent so it reads the rejection on its next run.
  const rejectDecision = useCallback(
    async (messageId: string, reason?: string) => {
      setBusyMessageId(messageId);
      clearError(messageId);
      try {
        const res = await api.rejectDecisionRequest(projectId, messageId, reason);
        setDecisions((cur) => {
          const existing = cur[messageId];
          if (!existing) return cur;
          return { ...cur, [messageId]: { ...existing, phase: res.status } };
        });
        onDecided?.();
      } catch (e) {
        setErrorByMessageId((cur) => ({ ...cur, [messageId]: decisionErrorMessage(e) }));
      } finally {
        setBusyMessageId(undefined);
      }
    },
    [api, projectId, onDecided, clearError],
  );

  const cards = Object.values(decisions);
  // No reachable thread, or a thread with no cards ⇒ render nothing. The decision
  // surface is silent until an agent actually asks (anti-nag, ADR-0026 §4.2) — unlike
  // TicketProposals' coordinator D3 hint, there is no "assign individually" affordance
  // to offer here, so an empty decisions section would be pure noise.
  if (cards.length === 0) {
    return null;
  }

  return (
    <section className="card ksq-ticket-decisions" data-testid="detail-decisions">
      <h2>Decisions</h2>
      <ul className="ksq-ticket-decisions__list" data-testid="detail-decisions-list">
        {cards.map((d) => (
          <li key={d.Message.id}>
            <DecisionCard
              decision={d}
              onAnswer={answerDecision}
              onReject={rejectDecision}
              busy={busyMessageId === d.Message.id}
            />
            {errorByMessageId[d.Message.id] ? (
              <p
                className="ksq-decision__error"
                role="alert"
                data-testid="decision-error"
              >
                {errorByMessageId[d.Message.id]}
              </p>
            ) : null}
          </li>
        ))}
      </ul>
    </section>
  );
}

export default TicketDecisions;
