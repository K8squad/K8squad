-- 0010_credential_pause.sql — the per-credential pause/resume ledger + the
-- reconcile_step enum extension for the credential pause family
-- (Stories 7.4+7.6 / ISI-2898, arch §10 pause/resume, §11 per-user credentials,
-- §7.2 credentialLifecycle). Gap-close for the ISI-2876 BMAD alignment review.
--
-- Forward-only companion to 0003_coord_outbox.sql and 0005_reconcile_step.sql.
-- The credential pause/resume machinery (pkg/coord/credpause.go,
-- coord.CredentialPauseStore, and the atomic shim-signal applier
-- pkg/coord/credsignal.go, coord.ApplyCredentialSignal) binds to the durable
-- substrate this migration adds. It reuses the Story 2.11/3.7 resume discipline
-- (single durable wake, crash-safe re-derivation, equal-jitter backoff, SKIP
-- LOCKED exactly-once claims — 0003/0005 + pkg/coord/resume.go) and extends it
-- along the two axes the credential stories add:
--
--   * ATTRIBUTION (7.6): episodes are keyed by CREDENTIAL (the per-user Secret
--       identity, arch §11), not by work item — so a rate-limit hold names the
--       exact principal/Secret that was throttled, two credentials on one item
--       keep independent Retry-After windows, and the coordinator re-route
--       advisory (2.10) can pick an Agent bound to a DIFFERENT, unpaused
--       credential (coord.SelectAlternate).
--   * REASON FAMILY + RESUME MODE (7.4): one pause machinery, four legible
--       reasons. rate_limited resumes on the single durable timer wake
--       (resume_at = now + Retry-After, §8 tier-2); credential_expired,
--       credential_rotated and endpoint_unreachable carry NO timer — they clear
--       via ResumeOnRefresh when the 7.7 credential controller / a Secret
--       rotation writes fresh material back. An unreachable BYO endpoint (7.5)
--       is therefore a legible Paused, never an opaque dial failure.
--
-- §6.6 outbox: ApplyCredentialSignal co-commits ONE coord.outbox row
-- (entity='run', event_type='paused') per applied signal in the SAME transaction
-- as the ledger episode and the guarded coord.claim.reconcile_step move — the
-- same AC6 no-divergence discipline as ProdReconcileStore.Advance. There is one
-- §6.6 outbox (ADR-023); this migration does NOT own a second one.

-- ---------------------------------------------------------------------------
-- (1) reconcile_step enum extension — the credential pause family
-- ---------------------------------------------------------------------------
-- The Story 3.1 CHECK (0005) pinned reconcile_step through 'paused(rate_limited)'.
-- Stories 7.4/7.6 add three sibling holds in the SAME pause/resume family. A
-- CHECK cannot be extended in place, so we DROP and re-ADD with the full closed
-- set. Keep this list in lockstep with pkg/reconcile/machine.go (Step consts and
-- reconcile.CredentialPauseSteps). Forward-only: the added values are a superset,
-- so no existing row can violate the new constraint.
ALTER TABLE coord.claim DROP CONSTRAINT claim_reconcile_step_enum;
ALTER TABLE coord.claim
    ADD CONSTRAINT claim_reconcile_step_enum CHECK (reconcile_step IN (
        'pending', 'claiming_sandbox', 'dispatching', 'running', 'collecting',
        'succeeded', 'failed', 'cancelled',
        'paused', 'paused(rate_limited)',
        'paused(credential_expired)', 'paused(credential_rotated)',
        'paused(endpoint_unreachable)'));

-- ---------------------------------------------------------------------------
-- (2) coord.credential_pause — the per-credential pause/resume ledger
-- ---------------------------------------------------------------------------
-- credential_id (the per-user Secret identity "namespace/name", arch §11) is the
-- PRIMARY KEY: a credential has AT MOST ONE live episode, so a re-signalled pause
-- rewrites the row in place (ON CONFLICT (credential_id) DO UPDATE) rather than
-- stacking duplicates — the attribution is idempotent per credential (7.6).
-- resumed_at NULL ⇔ the episode is PENDING (held); the two partial indexes below
-- keep the timer scan and the advisory read on the live working set only.
CREATE TABLE coord.credential_pause (
    credential_id    text        PRIMARY KEY,          -- per-user Secret "namespace/name" (arch §11) — the attribution key
    principal        text        NOT NULL DEFAULT '',  -- owning human seat / service principal (§6.5)
    credential_class text        NOT NULL              -- the story-pinned credential model (arch §10/§11)
        CONSTRAINT credential_pause_class_enum CHECK (
            credential_class IN ('claude_oauth', 'api_key', 'byo_endpoint')),
    reason           text        NOT NULL              -- the legible pause reason (one family, §10 / FR-G3)
        CONSTRAINT credential_pause_reason_enum CHECK (
            reason IN ('rate_limited', 'credential_expired',
                       'credential_rotated', 'endpoint_unreachable')),
    work_item_id     uuid        REFERENCES coord.work_item(id) ON DELETE SET NULL,  -- provenance: where the signal surfaced (nullable)
    run_id           uuid,                             -- provenance: the Run held when the signal fired (nullable)
    retry_after_ms   bigint,                           -- provider Retry-After window; rate_limited only, else NULL
    attempt          int         NOT NULL,             -- backoff streak (equal-jitter escalation, mirrors resume.go)
    resume_at        timestamptz,                      -- the single durable timer wake; NULL ⇔ refresh-mode hold (7.4)
    paused_at        timestamptz NOT NULL DEFAULT clock_timestamp(),  -- when the episode was recorded
    resumed_at       timestamptz,                      -- NULL ⇔ PENDING; stamped by ResumeDue (timer) or ResumeOnRefresh
    refreshed_at     timestamptz,                      -- stamped by ResumeOnRefresh (fresh credential material landed)

    -- 7.4 teeth: only rate_limited carries a timer. expiry/rotation/unreachable
    -- clear on refresh, so the platform never fabricates a clock wake for a hold
    -- that only fresh credential material can release.
    CONSTRAINT credential_pause_timer_only_rate_limited CHECK (
        reason = 'rate_limited' OR (resume_at IS NULL AND retry_after_ms IS NULL))
);

-- The timer's hot path (NextWake / ResumeDue): earliest PENDING timer deadline.
-- Partial index excludes resumed rows AND refresh-mode holds (resume_at NULL), so
-- the SKIP LOCKED claim scans only credentials actually waiting on a clock.
CREATE INDEX idx_credential_pause_due ON coord.credential_pause (resume_at)
    WHERE resumed_at IS NULL AND resume_at IS NOT NULL;

-- The advisory read (PausedSet → 2.10 re-route + 8.6/8.11 console): every PENDING
-- episode regardless of resume mode. Partial index keeps it to the held set.
CREATE INDEX idx_credential_pause_pending ON coord.credential_pause (credential_id)
    WHERE resumed_at IS NULL;
