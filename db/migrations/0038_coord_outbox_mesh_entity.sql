-- 0038_coord_outbox_mesh_entity.sql — admit the `mesh` subject-taxonomy entity
-- to the coord.outbox CHECK (AgentMesh strangler step S0, ISI-5833; approved
-- chapter hack/isi-5806-agent-mesh-chapter.md §13.4 I6, Henrik-locked f93795d5).
--
-- Forward-only, versioned migration (same discipline as 0001..0037: applied exactly once, in
-- filename order, by the apiserver migration runner / db.dbmigrate.Apply on startup; there is NO
-- down migration — a mistake is corrected by a new forward file. HEAD was 0037 per ISI-5744/5746).
--
-- WHY THIS EXISTS (§13.4 S0 — "telemetry FIRST, ships alone, additive, NO behavior change"):
-- The AgentMesh P1 self-telemetry (ISI-5833 I9) publishes mesh lifecycle events —
-- register | deregister | hibernate | wake | stop — onto the NATS subject spine
-- `ksquad.{entity}.{project}.{squad}.{event_type}` via the existing coord.outbox relay
-- (pkg/events). Those events belong to a NEW subject-taxonomy entity family, `mesh`: they are
-- facts about a standing role-provisioned agent process, not about a `run`/`work_item`/`artifact`.
-- The outbox entity column is a bounded CHECK (0003) so a typo'd entity cannot publish to a garbage
-- subject and silently mis-deliver; a new event source therefore extends the set via an ADDITIVE
-- migration (versioned event-catalog discipline, §10.2 / 0003 prose) — never an ad-hoc string.
--
-- This migration ships AHEAD of any mesh behavior (strangler S0): widening the CHECK is purely
-- permissive (it admits a value, forbids nothing that was legal before), so no existing row, write
-- path, or relay changes. The Go mirror (pkg/events.Entities) and the semconv contract
-- (pkg/telemetry/semconv) are widened in the same change set; the five mesh events stay
-- Stability:Planned until the Gateway/Supervisor that emits them lands (S2/S3). Rollback of the
-- whole mesh effort is a per-team flag flip (§13.4) — this admitted value simply goes unused, it
-- never needs unwinding.
--
-- NOTE on `pool_saturated`: it is deliberately NOT a mesh event here. Saturation is a sampled
-- threshold with no transactional anchor that flaps under an at-least-once relay and is already
-- derivable from the `mesh.processes.active{state=busy} == max` gauge — it is METRIC-ONLY (ObsAgent
-- schema-delta consult ISI-5813, finding (a)🔴). Five mesh events, not six.

-- ---------------------------------------------------------------------------
-- Widen the bounded entity CHECK: {run, work_item, artifact, memory, scm} → + mesh.
-- The 0003 inline column CHECK is named `outbox_entity_check` by Postgres (deterministic
-- <table>_<column>_check). DROP + re-ADD is the forward-only way to widen an inline CHECK (there is
-- no ALTER CHECK); IF EXISTS keeps the DROP idempotent if the constraint was ever renamed. The new
-- constraint is a strict superset, so the re-ADD validates instantly against existing rows (every
-- stored entity is already in the old, smaller set).
-- ---------------------------------------------------------------------------
ALTER TABLE coord.outbox DROP CONSTRAINT IF EXISTS outbox_entity_check;
ALTER TABLE coord.outbox
    ADD CONSTRAINT outbox_entity_check
    CHECK (entity IN ('run','work_item','artifact','memory','scm','mesh'));
