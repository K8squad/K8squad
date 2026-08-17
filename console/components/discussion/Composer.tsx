'use client';

// Composer — post a new message or reply-in-thread (AC3). It collects ONLY a
// body (and, for a reply, the parent id it was opened against). On submit it
// hands the input to `buildPostBody`, which is the single choke point
// guaranteeing the wire body is `{ body, parentId? }` and NOTHING else —
// provenance is server-stamped.
//
// AC5 / no-P2P: this is a COLLABORATION surface. The composer exposes no
// claim / checkout / assign / transition / complete affordance. Posting a
// message moves no work item and changes no coordination state.

import { useState } from 'react';
import {
  buildPostBody,
  canSubmit,
  type ComposerInput,
  type PostMessageBody,
} from '@/lib/discussion/compose';

export interface ComposerProps {
  /** When set, this composer replies in-thread to the given parent message. */
  parentId?: string | null;
  /** Receives the exact wire body — `{ body, parentId? }`, never any author. */
  onPost: (body: PostMessageBody) => void | Promise<void>;
  placeholder?: string;
}

export function Composer({ parentId, onPost, placeholder }: ComposerProps) {
  const [body, setBody] = useState('');
  const input: ComposerInput = { body, parentId: parentId ?? null };
  const disabled = !canSubmit(input);

  const submit = async () => {
    if (!canSubmit(input)) return;
    await onPost(buildPostBody(input));
    setBody('');
  };

  return (
    <form
      className="ksq-composer"
      data-testid="composer"
      data-reply-to={parentId ?? ''}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <textarea
        className="ksq-composer__body"
        data-testid="composer-body"
        aria-label={parentId ? 'Reply in thread' : 'Post a message'}
        placeholder={placeholder ?? (parentId ? 'Reply…' : 'Post to the room…')}
        value={body}
        onChange={(e) => setBody(e.target.value)}
      />
      <button
        type="submit"
        className="ksq-composer__submit"
        data-testid="composer-submit"
        disabled={disabled}
      >
        {parentId ? 'Reply' : 'Post'}
      </button>
    </form>
  );
}
