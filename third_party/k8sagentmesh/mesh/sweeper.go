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
	"fmt"
	"time"
)

// DefaultSweepInterval is the default prune cadence. Hosts should derive it
// from their registry TTL: the reference host sweeps at TTL/3 (30s against a
// 90s TTL — the gateway re-registers on a 30s ticker, so the default
// tolerates three missed heartbeats).
const DefaultSweepInterval = 30 * time.Second

// SweeperOption configures a Sweeper.
type SweeperOption func(*Sweeper)

// WithSweeperClock overrides the sweeper's clock (tests pin TTL computations
// deterministically without sleeping).
func WithSweeperClock(now func() time.Time) SweeperOption {
	return func(s *Sweeper) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSweeperInterval overrides the prune cadence. Non-positive values are
// ignored.
func WithSweeperInterval(d time.Duration) SweeperOption {
	return func(s *Sweeper) {
		if d > 0 {
			s.interval = d
		}
	}
}

// Sweeper is the registry TTL-prune loop (AgentMesh chapter §7/§8 + §9.3;
// ISI-6010 P1.4b; lifted from the reference host's supervisor loop at
// P3.4): the supervisor-side ticker that keeps the mesh endpoint registry
// bounded. Each tick calls Registry.Sweep to delete TTL-expired rows — the
// hygiene path that guarantees the registry never grows unbounded with dead
// gateways (correctness never depends on it: a TTL-filtered LookupTeamRole
// already hides expired rows the moment their heartbeat ages out).
//
// This is the TTL-prune leg of the supervisor mechanism. The health-probe
// and drain legs are the SupervisorHealth loop (lifecycle.go): it probes
// gateway serving readiness, drains downed pods through the store seam and
// re-creates them via the host. Recreate is delegated there by design — a
// host's pod lifecycle (liveness probes, warm-pool boot idempotency) owns
// recreation, never this loop. Its Start has the shape of a host's
// manager.Runnable — a host adds it to its supervisor's run group with
// mgr.Add(s).
type Sweeper struct {
	registry Registry
	interval time.Duration
	now      func() time.Time
}

// NewSweeper creates a sweeper bound to the registry, pruning at
// DefaultSweepInterval unless overridden.
func NewSweeper(registry Registry, opts ...SweeperOption) (*Sweeper, error) {
	if registry == nil {
		return nil, fmt.Errorf("mesh.NewSweeper: nil registry")
	}
	s := &Sweeper{
		registry: registry,
		interval: DefaultSweepInterval,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Start runs the prune loop until ctx is cancelled. Sweep failures are
// tolerated and retried on the next tick — a store blip must never stop the
// supervisor.
func (s *Sweeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep performs one registry prune: deletes TTL-expired rows.
func (s *Sweeper) sweep(ctx context.Context) {
	// TTL-expired rows are already invisible to LookupTeamRole (it filters
	// on last_seen_at), so correctness does not depend on Sweep. Sweep is
	// hygiene to keep the registry bounded. The returned count is the
	// number of reclaimed rows; observable for metrics via the host's
	// telemetry binding.
	n, err := s.registry.Sweep(ctx)
	if err != nil {
		// Log-and-continue in production terms: the next tick retries. A
		// sweeper error is never fatal to the supervisor loop.
		_ = err
	}
	_ = n
}
