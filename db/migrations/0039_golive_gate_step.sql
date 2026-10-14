-- 0039_golive_gate_step.sql — the reconcile_step enum extension for the WS-9 S5
-- go-live gate's pre-dispatch hold (ISI-5849, parent ISI-5818).
--
-- Forward-only companion to 0005_reconcile_step.sql, 0010_credential_pause.sql
-- and 0017_reconcile_cancel_step.sql. The S5 go-live gate (pkg/toolcred gate
-- oracle, PR #902) is the FIRST production consumer of the credential-pause
-- loop: a Run that grants a [sec]-gated Skill/MCPServer whose declared
-- credential type(s) are NOT materialized in the project namespace is withheld
-- PRE-DISPATCH and held at the new durable step 'paused(credential_required)'
-- (pkg/reconcile/machine.go — a member of CredentialPauseSteps, projecting onto
-- PhasePaused). Unlike the 7.4/7.6 holds (expired/rotated/rate_limited/
-- unreachable), this hold is admission-time and NOT per-credential-attributed:
-- it clears when a managed-credential Secret for a declared type lands and the
-- driver's pre-dispatch gate re-evaluates (coord.ApplyGateHold / ApplyGateResume).
--
-- A CHECK cannot be extended in place, so we DROP and re-ADD with the full
-- closed set — the same pattern 0010/0017 used. Keep this list in lockstep with
-- pkg/reconcile/machine.go (Step consts). Forward-only: the added value is a
-- superset, so no existing row can violate the new constraint. A database whose
-- constraint was extended by hand converges back onto this canonical definition
-- when the runner applies 0039 — the DROP/re-ADD is shape-agnostic about how the
-- live constraint got there.
--
-- The partial index idx_claim_reconcile_active (0005) keeps
-- 'paused(credential_required)' in its active scan set by construction: like
-- 'cancelling' (0017 note) a gate-held claim is NON-terminal and due for the
-- driver's resume re-kick when a credential Secret lands, so it belongs on the
-- failover re-entry working set. (The index predicate excludes only the
-- terminal set succeeded/failed/cancelled, so no index change is needed — the
-- new value is admitted into the active scan automatically.)
ALTER TABLE coord.claim DROP CONSTRAINT claim_reconcile_step_enum;
ALTER TABLE coord.claim
    ADD CONSTRAINT claim_reconcile_step_enum CHECK (reconcile_step IN (
        'pending', 'claiming_sandbox', 'dispatching', 'running', 'collecting',
        'succeeded', 'failed', 'cancelled', 'cancelling',
        'paused', 'paused(rate_limited)',
        'paused(credential_expired)', 'paused(credential_rotated)',
        'paused(endpoint_unreachable)', 'paused(credential_required)'));
