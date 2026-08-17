// MessageItem — one message row: provenance badge, body, timestamp, edited
// marker, and (recursively) its replies. Retracted messages render as a
// tombstone rather than being silently dropped (audit-honest, Story 10.3 §2).
//
// The landed 10.1 schema has no `invalidated_at` column; retraction is carried
// forward-compatibly via `metadata.retracted` (or a future `invalidatedAt`),
// so this renderer already honours the tombstone contract the story specifies.

import type { Message } from "../../lib/discussion/types";
import { deriveAuthorBadge } from "../../lib/discussion/provenance";
import { AuthorBadge } from "./AuthorBadge";

export function isRetracted(m: Message): boolean {
  const meta = m.metadata ?? {};
  return (
    meta["retracted"] === true ||
    typeof meta["invalidatedAt"] === "string" ||
    typeof meta["invalidated_at"] === "string"
  );
}

export function MessageItem({ message }: { message: Message }) {
  const retracted = isRetracted(message);
  const badge = deriveAuthorBadge(message);

  return (
    <li className="ksq-message" data-testid="message" data-message-id={message.id}>
      <div className="ksq-message__head">
        <AuthorBadge badge={badge} />
        <time className="ksq-message__ts" dateTime={message.createdAt}>
          {message.createdAt}
        </time>
        {message.editedAt ? (
          <span className="ksq-message__edited" data-testid="edited-marker">
            edited
          </span>
        ) : null}
      </div>

      {retracted ? (
        <p className="ksq-message__tombstone" data-testid="tombstone">
          <em>message retracted</em>
        </p>
      ) : (
        <p className="ksq-message__body">{message.body}</p>
      )}

      {message.replies && message.replies.length > 0 ? (
        <ul className="ksq-thread" data-testid="replies">
          {message.replies.map((r) => (
            <MessageItem key={r.id} message={r} />
          ))}
        </ul>
      ) : null}
    </li>
  );
}
