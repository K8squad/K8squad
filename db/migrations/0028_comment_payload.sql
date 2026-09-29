-- 0028_comment_payload.sql — ISI-5214 (parent ISI-5212 S2 / WS-B): carry structured ticket
-- references on a human work-item comment.
--
-- The discussion room persists a message's `#`-picker ticket links in Message.Payload under a
-- `references` JSON key (ISI-5165/ISI-5166: StampReferences / ReferencesOf on discussion.message.payload).
-- The ticket-detail composer now grows the same `#` affordance (dual triggerFragmentBefore primitive),
-- so a human comment on a ticket must be able to carry the same structured links back — but coord.comment
-- had no payload column to hang them on (only author_principal + body).
--
-- This adds the mirror column: a nullable jsonb `payload` on coord.comment, holding the SAME shape the
-- discussion side uses — `{"references":[{"workItemId":"…","title":"…"}]}`. It is additive, forward-only,
-- backfill-safe: every existing comment reads back NULL (no references), and any comment write that
-- collects no `#` links leaves it NULL, so a plain comment stays link-free exactly as before. A reference
-- is durable LINK metadata, never a dispatch and never a write fence (the comment row is the artifact).
ALTER TABLE coord.comment
    ADD COLUMN IF NOT EXISTS payload jsonb;
