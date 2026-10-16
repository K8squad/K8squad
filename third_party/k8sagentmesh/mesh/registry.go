/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"time"
)

// Endpoint is one registered standing process: the (team, role) pool key it
// serves, the pool pod its gateway runs in, the A2A base URL it serves
// (routes target <Endpoint>/task), and the announced agent card. AgentCard
// is the raw JSON the gateway registered (an a2a.AgentCard on the wire);
// the registry stores it opaquely and never interprets it.
type Endpoint struct {
	EndpointID   string
	Team         string
	Role         string
	Pod          string
	Endpoint     string
	AgentCard    []byte
	RegisteredAt time.Time
	LastSeenAt   time.Time
}

// Registry is the Supervisor-side mesh endpoint registry seam (AgentMesh
// chapter §5.1, D3.2; ISI-5822 P1.1; lifted P3.4): the store CONTRACT the
// registration ingress, the health-sweep loops and the route leg all
// speak. The module owns only the interface — every store stays host-side
// (k8squad binds it to its Postgres mesh-endpoint store; a second adopter
// binds its own equivalent).
//
// The registry is deliberately NOT a custody surface (D3.3): exclusivity
// stays in the host's claim guards and at-most-once stays in the host's
// dispatch marker. A row here only says "this process exists and serves" —
// never "this work is mine". That separation is what makes the mesh route
// reversible per team (flag off ⇒ the legacy path never reads the registry)
// and what keeps the at-most-once guarantee independent of HOW a task
// reached an executor.
//
// The write side gateways register/heartbeat/deregister through (via the
// Ingress), and the read side the supervisor's route leg and health loops
// consume. A host binds one authoritative store; the zero value of an
// implementation is not usable. Every operation must be idempotent — a
// gateway's registration beat may be replayed by retries, and a
// resurrecting gateway takes its rows back rather than shadowing them.
type Registry interface {
	// Register upserts one endpoint: a gateway registering its process on
	// boot, or re-registering after a restart — the row is taken back,
	// never shadowed (endpoint_id is the key; a resurrecting gateway with
	// the same id updates in place). last_seen_at resets to now, so a
	// re-registered endpoint is immediately routable again.
	Register(ctx context.Context, ep Endpoint) error

	// HeartbeatPod refreshes last_seen_at for every endpoint a pod's
	// gateway registered — the heartbeat unit is the GATEWAY (one call for
	// all its processes), matching the register wire. On the wire one
	// registration POST is both the initial registration and the
	// heartbeat (the ingress upserts each listed endpoint), so hosts that
	// separate the two refresh paths still converge to one row per
	// endpoint. Returns the number of refreshed rows so a gateway whose
	// registration vanished (sweeper race) can detect 0 and re-register
	// loudly.
	HeartbeatPod(ctx context.Context, pod string, endpointIDs []string) (int64, error)

	// PrunePod drops every endpoint of pod EXCEPT the listed ids — the
	// ingress's per-heartbeat reconciliation: a gateway that recreated a
	// process (new endpoint id) leaves the dead id behind, and pruning it
	// at the next registration beat keeps the registry honest without
	// waiting for the TTL. Idempotent; safe for a pod with no rows.
	PrunePod(ctx context.Context, pod string, keepIDs []string) error

	// DeregisterPod drops every endpoint a pod's gateway registered — the
	// clean shutdown path, and the drain the health sweep runs on a
	// downed gateway pod (chapter §2.4). Must be idempotent.
	DeregisterPod(ctx context.Context, pod string) error

	// LookupTeamRole returns the LIVE endpoints serving (team, role),
	// ordered least-recently-seen first (chapter §5.3 LRU tie-break:
	// spread wear and keep caches warm across the set). TTL-expired rows
	// are excluded by the store itself — a dead gateway's endpoints stop
	// being routable the moment their heartbeat ages out, with no
	// dependence on the sweeper.
	LookupTeamRole(ctx context.Context, team, role string) ([]Endpoint, error)

	// Sweep deletes TTL-expired rows — the reclaimer that keeps the
	// registry from growing with dead gateways. Correctness never depends
	// on it (Lookup filters by TTL anyway); it is hygiene. Returns the
	// number of rows deleted.
	Sweep(ctx context.Context) (int64, error)

	// ListEndpoints returns EVERY registered endpoint, live and
	// TTL-expired alike — the health loop's input (chapter §2.4).
	// LookupTeamRole's TTL filter would hide exactly the expired rows the
	// health loop exists to drain, so the loop reads this unfiltered view
	// and applies the TTL itself.
	ListEndpoints(ctx context.Context) ([]Endpoint, error)
}
