// ProposalCard — the dashed accent card for a kind='proposal' message
// (ISI-4930 story 6, plan §4.4/§4.7). It renders the authorizable action, the
// proposer's body, the lifecycle phase, and — while still `proposed` — the
// human confirm/dismiss verbs. The card is a pure consumer: it never executes,
// it only renders what the server gives it and hands confirm/dismiss back to
// the room, which fans into the existing authoring shells.

import type { Proposal, ProposalPayload } from "@/lib/discussion/types";

/** Human-readable one-liner for the action a proposal names (plan §6). */
export function proposalActionLabel(payload: ProposalPayload): string {
  switch (payload.action) {
    case "create_ticket":
      return payload.title
        ? `Create ticket "${payload.title}"`
        : "Create ticket";
    case "assign_agent":
      return payload.ticketId
        ? `Assign ${payload.assigneeAgentId ?? "an agent"} to ticket ${payload.ticketId}`
        : "Assign agent";
    case "party_run":
      return payload.title ? `Start a party run "${payload.title}"` : "Start a party run";
    default:
      return "Proposal";
  }
}

export interface ProposalCardProps {
  /** The proposal card joined with its lifecycle row (list-proposals). */
  proposal: Proposal;
  /** Human confirm verb — fans into the authoring seams (plan §4.4). */
  onConfirm?: (messageId: string) => void;
  /** Human dismiss verb — records the decision, no fan-out (plan §4.4). */
  onDismiss?: (messageId: string) => void;
  /** True while a confirm/dismiss round-trip is in flight. */
  busy?: boolean;
}

export function ProposalCard({
  proposal,
  onConfirm,
  onDismiss,
  busy,
}: ProposalCardProps) {
  const { phase } = proposal;
  const proposed = phase === "proposed";
  return (
    <div
      className="ksq-proposal"
      data-testid="proposal-card"
      data-phase={phase}
    >
      <div className="ksq-proposal__action" data-testid="proposal-action">
        {proposalActionLabel(proposal.Payload)}
      </div>
      {proposal.Message.body ? (
        <p className="ksq-proposal__body">{proposal.Message.body}</p>
      ) : null}
      <div className="ksq-proposal__footer">
        <span
          className="ksq-chip ksq-proposal__phase"
          data-testid="proposal-phase"
          data-phase={phase}
        >
          {phase}
        </span>
        {proposal.decidedBy ? (
          <span className="ksq-proposal__decided" data-testid="proposal-decided-by">
            by {proposal.decidedBy}
          </span>
        ) : null}
        {proposed ? (
          <div className="ksq-proposal__actions">
            <button
              type="button"
              className="ksq-proposal__btn ksq-proposal__btn--confirm"
              data-testid="proposal-confirm"
              disabled={busy}
              onClick={() => onConfirm?.(proposal.Message.id)}
            >
              Confirm
            </button>
            <button
              type="button"
              className="ksq-proposal__btn ksq-proposal__btn--dismiss"
              data-testid="proposal-dismiss"
              disabled={busy}
              onClick={() => onDismiss?.(proposal.Message.id)}
            >
              Dismiss
            </button>
          </div>
        ) : null}
      </div>
    </div>
  );
}
