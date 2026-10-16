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
	"sort"
	"sync"
	"time"
)

// fakeRegistry is the in-memory Registry the module tests bind — the store
// itself is host-side, and this is also the reference for how small a host
// binding can be: every op is a map pass with a last_seen_at TTL filter.
type fakeRegistry struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time

	byID map[string]Endpoint

	// failure injection per op (nil = succeed).
	errRegister   error
	errHeartbeat  error
	errPrune      error
	errDeregister error
	errLookup     error
	errSweep      error
	errList       error

	// call recording.
	registers  []Endpoint
	prunes     []pruneCall
	deregPods  []string
	sweeps     int
	heartbeats int
}

type pruneCall struct {
	pod  string
	keep []string
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		byID: map[string]Endpoint{},
		ttl:  90 * time.Second,
		now:  time.Now,
	}
}

func (f *fakeRegistry) Register(_ context.Context, ep Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errRegister != nil {
		return f.errRegister
	}
	prev, existed := f.byID[ep.EndpointID]
	if !existed {
		ep.RegisteredAt = f.now()
	} else {
		ep.RegisteredAt = prev.RegisteredAt
	}
	ep.LastSeenAt = f.now()
	f.byID[ep.EndpointID] = ep
	f.registers = append(f.registers, ep)
	return nil
}

func (f *fakeRegistry) HeartbeatPod(_ context.Context, _ string, endpointIDs []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errHeartbeat != nil {
		return 0, f.errHeartbeat
	}
	var n int64
	for _, id := range endpointIDs {
		if ep, ok := f.byID[id]; ok {
			ep.LastSeenAt = f.now()
			f.byID[id] = ep
			n++
		}
	}
	f.heartbeats++
	return n, nil
}

func (f *fakeRegistry) PrunePod(_ context.Context, pod string, keepIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errPrune != nil {
		return f.errPrune
	}
	keep := map[string]struct{}{}
	for _, id := range keepIDs {
		keep[id] = struct{}{}
	}
	for id, ep := range f.byID {
		if ep.Pod == pod {
			if _, ok := keep[id]; !ok {
				delete(f.byID, id)
			}
		}
	}
	f.prunes = append(f.prunes, pruneCall{pod: pod, keep: keepIDs})
	return nil
}

func (f *fakeRegistry) DeregisterPod(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errDeregister != nil {
		return f.errDeregister
	}
	for id, ep := range f.byID {
		if ep.Pod == pod {
			delete(f.byID, id)
		}
	}
	f.deregPods = append(f.deregPods, pod)
	return nil
}

func (f *fakeRegistry) LookupTeamRole(_ context.Context, team, role string) ([]Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errLookup != nil {
		return nil, f.errLookup
	}
	cutoff := f.now().Add(-f.ttl)
	var out []Endpoint
	for _, ep := range f.byID {
		if ep.Team == team && ep.Role == role && ep.LastSeenAt.After(cutoff) {
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.Before(out[j].LastSeenAt) })
	return out, nil
}

func (f *fakeRegistry) Sweep(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++ // call counter (attempts, incl. failures)
	if f.errSweep != nil {
		return 0, f.errSweep
	}
	cutoff := f.now().Add(-f.ttl)
	var n int64
	for id, ep := range f.byID {
		if !ep.LastSeenAt.After(cutoff) {
			delete(f.byID, id)
			n++
		}
	}
	return n, nil
}

func (f *fakeRegistry) ListEndpoints(_ context.Context) ([]Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errList != nil {
		return nil, f.errList
	}
	out := make([]Endpoint, 0, len(f.byID))
	for _, ep := range f.byID {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].EndpointID < out[j].EndpointID
	})
	return out, nil
}

// sweepCount reports the number of Sweep calls (thread-safe read for the
// sweeper loop tests).
func (f *fakeRegistry) sweepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

// get returns a copy of one row by endpoint id (test assertions).
func (f *fakeRegistry) get(endpointID string) (Endpoint, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ep, ok := f.byID[endpointID]
	return ep, ok
}
