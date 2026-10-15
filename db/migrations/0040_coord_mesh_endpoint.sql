-- 0040_coord_mesh_endpoint.sql — the MESH ENDPOINT REGISTRY (AgentMesh chapter §5.1, D3.2; ISI-5822 P1.1).
--
-- Forward-only, versioned migration (same discipline as 0001..0039: applied exactly once, in
-- filename order, by the apiserver migration runner / db.dbmigrate.Apply on startup; there is NO
-- down migration — a mistake is corrected by a new forward file. HEAD was 0039 per ISI-5833).
--
-- WHY THIS TABLE EXISTS (chapter §2.4 D1.3 + §5.2 D3.2):
-- The AgentMesh Supervisor (the evolved rundrive Driver) routes a Run by a REGISTRY LOOKUP —
-- (team, role) → the standing processes a Gateway (the evolved shim supervisor, PID1 of a pool
-- pod) has registered — instead of the legacy claim-for-execution pop-and-bind. The authoritative
-- registry lives in Postgres, NOT in gateway memory, so it survives a gateway restart: on restart
-- the gateway re-registers, and stale endpoints expire by TTL (§2.4). This table is that registry:
-- the "service discovery" of the mesh.
--
-- MUTABLE, NOT append-only: unlike coord.audit_log / run_phase_transition, a registration row is
-- LIVED-IN state — Register upserts, Heartbeat refreshes last_seen_at, Deregister deletes, and the
-- TTL-expired sweeper deletes cold rows. No append-only triggers here by design (a resurrecting
-- endpoint must be able to take its row back). Custody is NOT expressed here: exclusivity stays
-- in coord.claim (AcquireSpecific) and at-most-once stays in coord.a2a_dispatch — both UNCHANGED
-- by the mesh path (D3.3: route replaces claim-FOR-EXECUTION, never exclusivity).
--
-- COLUMN NOTES:
--   endpoint_id   — the gateway-assigned stable id of ONE standing process (adapter handle id).
--                   Globally unique (it embeds pod + process identity), so it is the PK.
--   team / role   — the (team, role) pool key the Supervisor's dispatch lookup filters on. Text
--                   names (Team/Role CR .metadata.name), matching how buildTask stamps
--                   envelope metadata "team" today.
--   pod           — the pool pod the registering gateway runs in (HOSTNAME in-cluster). The
--                   Supervisor drains by pod when it recreates an unhealthy gateway (§2.4).
--   endpoint      — the A2A base URL the process serves (http://<ip>:<port>); Route targets its
--                   /task surface. Also unique: two registry rows must never point at the same
--                   address — a re-registering gateway upserts, it never shadows.
--   agent_card    — the announced capability contract (skills/model/identity) the process serves,
--                   JSON of a2a.AgentCard. Read by the Supervisor for selection observability.
--   last_seen_at  — the registration/heartbeat clock; TTL-expired rows are dropped by the
--                   sweeper (lookup filters on it regardless, so a dead gateway's endpoints
--                   never receive a route even before the sweeper runs).

CREATE TABLE IF NOT EXISTS coord.mesh_endpoint (
    endpoint_id   text        PRIMARY KEY,             -- gateway-assigned stable process id (adapter handle id)
    team          text        NOT NULL,                -- Team CR name (the (team, role) pool key, dispatch lookup)
    role          text        NOT NULL,                -- Role CR name (e.g. "coder" for the P1.1 thin thread)
    pod           text        NOT NULL,                -- pool pod the gateway runs in (drain-by-pod key, §2.4)
    endpoint      text        NOT NULL UNIQUE,         -- A2A base URL (http://ip:port); Route POSTs <endpoint>/task
    agent_card    jsonb       NOT NULL,                -- announced a2a.AgentCard (skills/model/identity)
    registered_at timestamptz NOT NULL DEFAULT now(),  -- first registration of this endpoint_id
    last_seen_at  timestamptz NOT NULL DEFAULT now(),  -- heartbeat; TTL-expired ⇒ unrouteable + swept
    UNIQUE (team, role, pod, endpoint_id)              -- one gateway owns its (team, role, pod) ids outright
);

-- The dispatch lookup: (team, role) → live endpoints, LRU-ordered by last_seen_at (§5.3 selection).
CREATE INDEX IF NOT EXISTS mesh_endpoint_team_role_seen_idx
    ON coord.mesh_endpoint (team, role, last_seen_at);

-- Drain-by-pod (§2.4 unhealthy-gateway path) and the TTL sweep both key on pod.
CREATE INDEX IF NOT EXISTS mesh_endpoint_pod_idx
    ON coord.mesh_endpoint (pod);

-- TTL sweeper support: cold-row eviction scans last_seen_at ascending.
CREATE INDEX IF NOT EXISTS mesh_endpoint_last_seen_idx
    ON coord.mesh_endpoint (last_seen_at);
