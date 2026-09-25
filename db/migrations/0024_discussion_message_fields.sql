-- 0024_discussion_message_fields.sql — ISI-4925: Add audience, kind, and payload to discussion.message
--
-- Forward-only migration to add message type fields for ISI-4925. This enables:
--   - audience: 'party' | 'direct:{agentId}' for visibility control
--   - kind: 'text' | structured message type (future extensibility)
--   - payload: jsonb for structured message data
--
-- Existing rows default to 'party' audience and 'text' kind. Payload is nullable.
--
-- This is additive-only: the change preserves all existing behavior and data.
-- Same discipline as workitemcomment.go (additive extension).
--

-- Add the new columns to discussion.message
ALTER TABLE discussion.message
    ADD COLUMN audience text NOT NULL DEFAULT 'party',
    ADD COLUMN kind text NOT NULL DEFAULT 'text',
    ADD COLUMN payload jsonb;

-- Add index for audience filtering (important for visibility predicates)
CREATE INDEX idx_message_audience ON discussion.message (audience);

-- Add index for kind filtering (for future query optimization)
CREATE INDEX idx_message_kind ON discussion.message (kind);

-- Ensure audience values are constrained to the allowed set
ALTER TABLE discussion.message
    ADD CONSTRAINT audience_must_be_party_or_direct CHECK (
        audience = 'party' OR audience LIKE 'direct:%'
    );

-- Ensure kind has reasonable defaults (can be extended in future)
ALTER TABLE discussion.message
    ADD CONSTRAINT kind_must_be_text_or_extension CHECK (
        kind = 'text' OR kind IN ('structured', 'task', 'decision', 'vote')  -- extend as needed
    );