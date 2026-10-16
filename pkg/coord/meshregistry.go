/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// meshregistry.go — the Supervisor-side mesh endpoint registry (AgentMesh
// chapter §5.1, D3.2; ISI-5822 P1.1): the Postgres-backed service discovery
// the evolved rundrive Driver routes Runs by. A Gateway (the evolved shim
// supervisor, PID1 of a pool pod) registers the A2A endpoints of the standing
// processes it manages and heartbeats them; the Supervisor's dispatch leg
// looks (team, role) up here and routes to a live endpoint.
//
// The registry is deliberately NOT a custody surface (D3.3): exclusivity
// stays in coord.claim (AcquireSpecific) and at-most-once stays in
// coord.a2a_dispatch. A row here only says "this process exists and serves" —
// never "this work is mine". That separation is what makes the mesh route
// reversible per team (flag off ⇒ the legacy claim path never reads this
// table) and what keeps the at-most-once guarantee independent of HOW a task
// reached an executor.
package coord

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// MeshEndpointTTL is how long an endpoint stays routable after its last
// heartbeat (chapter §2.4: "stale endpoints expire by TTL"). The gateway
// re-registers on a 30s ticker, so the default tolerates three missed
// heartbeats — a gateway that dies stops being routable inside ~90s even if
// the sweeper never runs, because Lookup filters on last_seen_at regardless.
const MeshEndpointTTL = 90 * time.Second

// MeshEndpoint is one registered standing process: the (team, role) pool key
// it serves, the pool pod its gateway runs in, the A2A base URL it serves
// (Route targets <Endpoint>/task), and the announced agent card. AgentCard is
// the raw JSON the gateway registered (an a2a.AgentCard on the wire); the
// registry stores it opaquely and never interprets it.
type MeshEndpoint struct {
	EndpointID   string
	Team         string
	Role         string
	Pod          string
	Endpoint     string
	AgentCard    []byte
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

// MeshRegistry is the coord.mesh_endpoint store: the write side gateways
// register/heartbeat/deregister through, and the read side the Supervisor's
// route leg looks (team, role) up through. The zero value is not usable —
// construct with NewMeshRegistry.
type MeshRegistry struct {
	db  *sql.DB
	now func() time.Time
	ttl time.Duration
}

// MeshRegistryOption configures a MeshRegistry (test seams only so far).
type MeshRegistryOption func(*MeshRegistry)

// WithMeshClock overrides the registry clock (tests pin last_seen_at aging
// without sleeping). Production leaves the default time.Now.
func WithMeshClock(now func() time.Time) MeshRegistryOption {
	return func(r *MeshRegistry) { r.now = now }
}

// WithMeshTTL overrides the routability TTL (tests shrink it). Non-positive
// values are ignored.
func WithMeshTTL(ttl time.Duration) MeshRegistryOption {
	return func(r *MeshRegistry) {
		if ttl > 0 {
			r.ttl = ttl
		}
	}
}

// NewMeshRegistry binds the registry to the coordination Postgres.
func NewMeshRegistry(db *sql.DB, opts ...MeshRegistryOption) (*MeshRegistry, error) {
	if db == nil {
		return nil, fmt.Errorf("coord.NewMeshRegistry: nil db")
	}
	r := &MeshRegistry{db: db, now: time.Now, ttl: MeshEndpointTTL}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Register upserts one endpoint: a gateway registering its process on boot,
// or re-registering after a restart — the row is taken back, never shadowed
// (endpoint_id is the PK; a resurrecting gateway with the same id updates in
// place). last_seen_at resets to now, so a re-registered endpoint is
// immediately routable again. The whole pod's registration is idempotent:
// Register, Heartbeat and Register again converges to one row per endpoint.
func (r *MeshRegistry) Register(ctx context.Context, ep MeshEndpoint) error {
	if ep.EndpointID == "" || ep.Team == "" || ep.Role == "" || ep.Pod == "" || ep.Endpoint == "" {
		return fmt.Errorf("coord.MeshRegistry.Register: endpoint_id, team, role, pod and endpoint are all required")
	}
	if len(ep.AgentCard) == 0 {
		return fmt.Errorf("coord.MeshRegistry.Register: agent_card is required (the announced capability contract)")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO coord.mesh_endpoint (endpoint_id, team, role, pod, endpoint, agent_card, registered_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, now(), now())
		ON CONFLICT (endpoint_id) DO UPDATE SET
			team         = EXCLUDED.team,
			role         = EXCLUDED.role,
			pod          = EXCLUDED.pod,
			endpoint     = EXCLUDED.endpoint,
			agent_card   = EXCLUDED.agent_card,
			last_seen_at = now()`,
		ep.EndpointID, ep.Team, ep.Role, ep.Pod, ep.Endpoint, string(ep.AgentCard))
	if err != nil {
		return fmt.Errorf("coord.MeshRegistry.Register %s: %w", ep.EndpointID, err)
	}
	return nil
}

// HeartbeatPod refreshes last_seen_at for every endpoint a pod's gateway
// registered — the heartbeat unit is the GATEWAY (one RPC for all its
// processes), matching the register wire. Rows for endpoints the gateway no
// longer lists are not touched here; they age out by TTL/Sweep unless
// re-registered. Returns the number of refreshed rows so a gateway whose
// registration vanished (sweeper race) can detect 0 and re-register loudly.
func (r *MeshRegistry) HeartbeatPod(ctx context.Context, pod string, endpointIDs []string) (int64, error) {
	if pod == "" {
		return 0, fmt.Errorf("coord.MeshRegistry.HeartbeatPod: pod is required")
	}
	// Endpoint ids ride the pod scoping: the gateway may only heartbeat rows
	// in its own pod namespace of the registry, never another pod's.
	// pq.Array renders the ids as a Postgres array literal — the driver-neutral
	// encoding (pgx stdlib and lib/pq both accept it, and sqlmock can match it).
	res, err := r.db.ExecContext(ctx, `
		UPDATE coord.mesh_endpoint SET last_seen_at = now()
		 WHERE pod = $1 AND endpoint_id = ANY($2)`,
		pod, pq.Array(endpointIDs))
	if err != nil {
		return 0, fmt.Errorf("coord.MeshRegistry.HeartbeatPod %s: %w", pod, err)
	}
	return res.RowsAffected()
}

// PrunePod drops every endpoint of pod EXCEPT the listed ids — the ingress's
// per-heartbeat reconciliation: a gateway that recreated a process (new
// endpoint id) leaves the dead id behind, and pruning it at the next
// registration beat keeps the registry honest without waiting for the TTL.
// Idempotent; safe for a pod with no rows.
func (r *MeshRegistry) PrunePod(ctx context.Context, pod string, keepIDs []string) error {
	if pod == "" {
		return fmt.Errorf("coord.MeshRegistry.PrunePod: pod is required")
	}
	// pq.Array renders the keep-set as a Postgres array literal (driver-neutral
	// encoding across pgx stdlib / lib/pq / sqlmock).
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM coord.mesh_endpoint
		 WHERE pod = $1 AND NOT (endpoint_id = ANY($2))`,
		pod, pq.Array(keepIDs))
	if err != nil {
		return fmt.Errorf("coord.MeshRegistry.PrunePod %s: %w", pod, err)
	}
	return nil
}

// DeregisterPod drops every endpoint a pod's gateway registered — the clean
// shutdown path, and the drain the Supervisor runs before asking the
// Scheduler to recreate an unhealthy gateway's pod (chapter §2.4).
func (r *MeshRegistry) DeregisterPod(ctx context.Context, pod string) error {
	if pod == "" {
		return fmt.Errorf("coord.MeshRegistry.DeregisterPod: pod is required")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM coord.mesh_endpoint WHERE pod = $1`, pod)
	if err != nil {
		return fmt.Errorf("coord.MeshRegistry.DeregisterPod %s: %w", pod, err)
	}
	return nil
}

// LookupTeamRole returns the LIVE endpoints serving (team, role), ordered
// least-recently-seen first (the chapter §5.3 LRU tie-break: spread wear and
// keep caches warm across the set). TTL-expired rows are excluded by the
// predicate itself — a dead gateway's endpoints stop being routable the
// moment their heartbeat ages out, with no dependence on the sweeper.
func (r *MeshRegistry) LookupTeamRole(ctx context.Context, team, role string) ([]MeshEndpoint, error) {
	if team == "" || role == "" {
		return nil, fmt.Errorf("coord.MeshRegistry.LookupTeamRole: team and role are required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT endpoint_id, team, role, pod, endpoint, agent_card, registered_at, last_seen_at
		  FROM coord.mesh_endpoint
		 WHERE team = $1 AND role = $2 AND last_seen_at > $3
		 ORDER BY last_seen_at ASC`, team, role, r.now().Add(-r.ttl))
	if err != nil {
		return nil, fmt.Errorf("coord.MeshRegistry.LookupTeamRole %s/%s: %w", team, role, err)
	}
	defer rows.Close()
	var out []MeshEndpoint
	for rows.Next() {
		var ep MeshEndpoint
		if err := rows.Scan(&ep.EndpointID, &ep.Team, &ep.Role, &ep.Pod, &ep.Endpoint, &ep.AgentCard, &ep.RegisteredAt, &ep.LastSeenAt); err != nil {
			return nil, fmt.Errorf("coord.MeshRegistry.LookupTeamRole %s/%s: %w", team, role, err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// Sweep deletes TTL-expired rows — the reclaimer that keeps the table from
// growing with dead gateways. Correctness never depends on it (Lookup filters
// by TTL anyway); it is hygiene. Returns the number of rows deleted.
func (r *MeshRegistry) Sweep(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM coord.mesh_endpoint WHERE last_seen_at < $1`, r.now().Add(-r.ttl))
	if err != nil {
		return 0, fmt.Errorf("coord.MeshRegistry.Sweep: %w", err)
	}
	return res.RowsAffected()
}

// ListEndpoints returns EVERY registered endpoint, live and TTL-expired
// alike, ordered by (pod, endpoint_id) — the supervisor health loop's input
// (P1.4, ISI-6010, chapter §2.4). LookupTeamRole's TTL filter would hide
// exactly the expired rows the health loop exists to drain, so the loop reads
// through this unfiltered view and applies the TTL itself.
func (r *MeshRegistry) ListEndpoints(ctx context.Context) ([]MeshEndpoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT endpoint_id, team, role, pod, endpoint, agent_card, registered_at, last_seen_at
		  FROM coord.mesh_endpoint
		 ORDER BY pod, endpoint_id`)
	if err != nil {
		return nil, fmt.Errorf("coord.MeshRegistry.ListEndpoints: %w", err)
	}
	defer rows.Close()
	var out []MeshEndpoint
	for rows.Next() {
		var ep MeshEndpoint
		if err := rows.Scan(&ep.EndpointID, &ep.Team, &ep.Role, &ep.Pod, &ep.Endpoint, &ep.AgentCard, &ep.RegisteredAt, &ep.LastSeenAt); err != nil {
			return nil, fmt.Errorf("coord.MeshRegistry.ListEndpoints: %w", err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}
