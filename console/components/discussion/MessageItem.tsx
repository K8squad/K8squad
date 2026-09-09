// MessageItem — one message row: provenance badge, body, timestamp, and
// (recursively) its replies. Retracted messages render as a tombstone rather
// than being silently dropped (audit-honest, Story 10.3 §2).
//
// Retraction is the server-stamped soft-delete column `invalidatedAt`
// (`internal/discussion/store.go#Message.Retracted()` — ISI-4016); a present
// timestamp is the tombstone signal.

import type { Message } from "@/lib/discussion/types";
import { deriveAuthorBadge } from "@/lib/discussion/provenance";
import { AuthorBadge } from "./AuthorBadge";

export function isRetracted(m: Message): boolean {
  return typeof m.invalidatedAt === "string" && m.invalidatedAt !== "";
}

export function MessageItem({ message }: { message: Message }) {
  const retracted = isRetracted(message);
  const badge = deriveAuthorBadge(message);

  return (
    <li
      className="ksq-message"
      data-testid="message"
      data-message-id={message.id}
    >
      <div className="ksq-message__head">
        <AuthorBadge badge={badge} />
        <time className="ksq-message__ts" dateTime={message.createdAt}>
          {message.createdAt}
        </time>
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
