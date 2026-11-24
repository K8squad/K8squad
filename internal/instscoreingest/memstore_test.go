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

package instscoreingest

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is the in-memory Store for unit tests: it mirrors SQLStore's
// semantics (append-only window rows, upsert verdicts/scores, prune) so the
// service tests exercise orchestration, not SQL.
type memStore struct {
	mu        sync.Mutex
	window    []WindowRow
	verdicts  map[string]map[string]VerdictRow
	scores    map[string]ScoreRow
	evalAt    map[string]time.Time
	appends   int
	prunes    int
	loadErr   error
	appendErr error
}

func newMemStore() *memStore {
	return &memStore{
		verdicts: map[string]map[string]VerdictRow{},
		scores:   map[string]ScoreRow{},
		evalAt:   map[string]time.Time{},
	}
}

func (m *memStore) AppendWindow(_ context.Context, rows []WindowRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	m.window = append(m.window, rows...)
	m.appends++
	return nil
}

func (m *memStore) LoadWindow(_ context.Context, serviceName string, since time.Time) ([]WindowRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	var out []WindowRow
	for _, r := range m.window {
		if r.ServiceName == serviceName && r.ReceivedAt.After(since) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].Signal < out[j].Signal
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	return out, nil
}

func (m *memStore) LatestEvaluatedAt(_ context.Context, serviceName string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.evalAt[serviceName]
	return at, ok, nil
}

func (m *memStore) SaveEvaluation(_ context.Context, serviceName string, verdicts []VerdictRow, score ScoreRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	byRule := m.verdicts[serviceName]
	if byRule == nil {
		byRule = map[string]VerdictRow{}
		m.verdicts[serviceName] = byRule
	}
	for _, v := range verdicts {
		byRule[v.RuleID] = v
	}
	m.scores[serviceName] = score
	m.evalAt[serviceName] = score.EvaluatedAt
	return nil
}

func (m *memStore) PruneBefore(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.window[:0]
	var n int64
	for _, r := range m.window {
		if r.ReceivedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.window = kept
	m.prunes++
	return n, nil
}
