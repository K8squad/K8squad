// MessageItem — one message row: provenance badge, body, timestamp, and
// (recursively) its replies. Retracted messages render as a tombstone rather
// than being silently dropped (audit-honest, Story 10.3 §2).
//
// Retraction is the server-stamped soft-delete column `invalidatedAt`
// (`internal/discussion/store.go#Message.Retracted()` — ISI-4016); a present
// timestamp is the tombstone signal.
//
// Proposal messages (kind='proposal', ISI-4930 story 6, plan §4.4/§4.7) render
// as the dashed accent card; their lifecycle phase is joined from the
// list-proposals read (the transcript stays phase-less). The result post-back
// (kind='structured') renders a run chip that links the fan-out outcome back to
// its work item.

import type { Message, Proposal, ProposalResult } from "@/lib/discussion/types";
import { deriveAuthorBadge } from "@/lib/discussion/provenance";
import { parseAudience } from "@/lib/discussion/audience";
import { reviewWorkItemHref } from "@/lib/github-links";
import { AuthorBadge } from "./AuthorBadge";
import { ProposalCard } from "./ProposalCard";

export function isRetracted(m: Message): boolean {
  return typeof m.invalidatedAt === "string" && m.invalidatedAt !== "";
}

/** Best-effort parse of a structured post-back payload into its result shape. */
export function parseProposalResult(payload: unknown): ProposalResult | null {
  if (!payload || typeof payload !== "object") return null;
  const p = payload as Record<string, unknown>;
  if (typeof p.action !== "string") return null;
  return payload as ProposalResult;
}

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

export interface MessageItemProps {
  message: Message;
  /** Project id, used to deep-link the result post-back chip to its work item. */
  projectId?: string;
  /** messageId → joined proposal card (list-proposals). Absent ⇒ plain render. */
  proposalByMessageId?: Readonly<Record<string, Proposal>>;
  /** Human confirm verb on a proposal card (plan §4.4). */
  onConfirmProposal?: (messageId: string) => void;
  /** Human dismiss verb on a proposal card (plan §4.4). */
  onDismissProposal?: (messageId: string) => void;
  /** Message id whose confirm/dismiss round-trip is in flight. */
  busyMessageId?: string;
}

export function MessageItem({
  message,
  projectId,
  proposalByMessageId,
  onConfirmProposal,
  onDismissProposal,
  busyMessageId,
}: MessageItemProps) {
  const retracted = isRetracted(message);
  const badge = deriveAuthorBadge(message);
  const audience = parseAudience(message.audience);

  const isProposal = message.kind === "proposal";
  const proposal = isProposal
    ? proposalByMessageId?.[message.id]
    : undefined;

  // The result post-back is a kind='structured' reply parented to a proposal
  // card; its payload carries the fan-out outcome (workItemId) the run chip
  // links back to (plan §4.7).
  const isPostBack = message.kind === "structured";
  const result = isPostBack ? parseProposalResult(message.payload) : null;

  return (
    <li
      className="ksq-message"
      data-testid="message"
      data-message-id={message.id}
      data-audience={audience.kind}
      data-kind={message.kind ?? "text"}
    >
      <div className="ksq-message__head">
        <AuthorBadge badge={badge} />
        {audience.kind === "direct" ? (
          <span
            className="ksq-chip ksq-chip--direct"
            data-testid="audience-chip"
            title={`Direct to ${audience.agentId}`}
          >
            direct
          </span>
        ) : null}
        <time className="ksq-message__ts" dateTime={message.createdAt}>
          {message.createdAt}
        </time>
      </div>

      {retracted ? (
        <p className="ksq-message__tombstone" data-testid="tombstone">
          <em>message retracted</em>
        </p>
      ) : proposal ? (
        <ProposalCard
          proposal={proposal}
          onConfirm={onConfirmProposal}
          onDismiss={onDismissProposal}
          busy={busyMessageId === message.id}
        />
      ) : (
        <>
          <p className="ksq-message__body">{message.body}</p>
          {result?.workItemId ? (
            <a
              className="ksq-chip ksq-result-chip"
              data-testid="result-chip"
              href={reviewWorkItemHref(projectId ?? "", result.workItemId)}
              title={`Work item ${result.workItemId}`}
            >
              ticket {shortId(result.workItemId)}
            </a>
          ) : null}
        </>
      )}

      {message.replies && message.replies.length > 0 ? (
        <ul className="ksq-thread" data-testid="replies">
          {message.replies.map((r) => (
            <MessageItem
              key={r.id}
              message={r}
              projectId={projectId}
              proposalByMessageId={proposalByMessageId}
              onConfirmProposal={onConfirmProposal}
              onDismissProposal={onDismissProposal}
              busyMessageId={busyMessageId}
            />
          ))}
        </ul>
      ) : null}
    </li>
  );
}
