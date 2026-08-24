-- 0009_rate_limit_reroute.sql — durable throttled-credential hold for the
-- Story 2.10 rate-limit re-route (ISI-2882, gap ISI-2876 / arch §8 tier-1
-- recovery, §6.2/§6.3 custody, 3.7 escalation, 7.6 per-credential attribution,
-- seat-model §11.2/ADR-041).
--
-- Forward-only companion to 0001_coord_schema.sql. Story 2.10 adds NO new
-- custody surface: a re-route is fenced RELEASE → coordinator RE-DISPATCH →
-- §6.2 CLAIM (same discipline as reclaim 2.4 and handoff 2.8; never a P2P
-- handoff of the lease). The release (coord.ProdRerouteStore.ReleaseForReroute,
-- pkg/coord/prodreroute.go) bumps coord.claim.fence_token, clears the holder and
-- moves the item back to the todo lane — all on the 0001 tables.
--
-- What THIS migration adds is the one durable fact those tables cannot hold:
-- WHICH credential was throttled when the checkout was released. It is the
-- §6.2-claim-side guard input — while the hold is live (resume_at still in the
-- future), a claim presented by an Agent bound to the SAME credential is
-- rejected, so the re-routed item can only be picked up by an Agent with a
-- DIFFERENT credential (7.6). When the Retry-After window clears, the hold is
-- inert by predicate (any credential may claim again — the 3.7 auto-resume
-- branch), so no sweeper is load-bearing for correctness.
--
-- SEAT-MODE (§11.2/ADR-041): throttled_credential is an OPAQUE credential key
-- (the credential's identity, e.g. its Secret ref identity — supplied by the
-- caller that resolved Agent → credential). The re-route NEVER re-points one
-- human-seat token at a different principal: the new claimant always presents
-- its OWN credential (each Agent binds its own Secret ref, 7.2/7.8); this table
-- only records the identity that was throttled so the same-credential claim can
-- be refused. No token, secret or principal-pairing is stored here.
--
-- FR-B3/no-P2P: this is not a channel. It stores no content, carries no message,
-- and nothing here re-enters coordination — it is the routing-intent marker the
-- §6.2 guard reads, plus §6.5-observable provenance of the release decision.

-- ---------------------------------------------------------------------------
-- coord.rate_limit_reroute — one live hold per work item (claim-row discipline:
-- rewritten in place, never appended per episode)
-- ---------------------------------------------------------------------------
CREATE TABLE coord.rate_limit_reroute (
    work_item_id         uuid        PRIMARY KEY REFERENCES coord.work_item(id) ON DELETE CASCADE,
    throttled_credential text        NOT NULL,  -- opaque credential key that was rate-limited (7.6)
    attempt              int         NOT NULL,  -- escalation attempt that fired the re-route (3.7)
    resume_at            timestamptz NOT NULL,  -- Retry-After window clear; hold is inert past this
    released_fence       bigint      NOT NULL,  -- coord.claim.fence_token AFTER the fenced release bump
    released_run         uuid        NOT NULL,  -- the paused Run whose checkout was released
    coordinator          text        NOT NULL,  -- the 2.9 coordinator principal that decided the re-dispatch (§6.5)
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- A subsequent escalation episode (the item paused again on a re-claim) rewrites
-- the row in place; updated_at rides the canonical 0001 touch trigger.
CREATE TRIGGER rate_limit_reroute_touch_updated_at
    BEFORE UPDATE ON coord.rate_limit_reroute
    FOR EACH ROW EXECUTE FUNCTION coord.touch_updated_at();
