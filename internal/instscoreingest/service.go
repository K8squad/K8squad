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
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/K8squad/K8squad/pkg/telemetry/instscore"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// Defaults per the ISI-7741 plan: the spec's sliding window (30 days) is
// retention; re-evaluation is throttled so a chatty service does not re-fold
// its whole history on every gateway batch.
const (
	DefaultWindow     = 30 * 24 * time.Hour
	DefaultMinReeval  = 10 * time.Minute
	DefaultPruneEvery = time.Hour
)

// ruleImpact pins rule ID → impact once, from the engine's registry (the
// rulesync test already guards the registry against upstream drift).
var ruleImpact = func() map[string]string {
	m := make(map[string]string, len(instscore.Rules))
	for _, r := range instscore.Rules {
		m[r.ID] = string(r.Impact)
	}
	return m
}()

// Service orchestrates ingestion: split → persist → (throttled) re-evaluate.
// The zero value is not usable; build with New.
type Service struct {
	store  Store
	logger *slog.Logger

	window     time.Duration
	minReeval  time.Duration
	pruneEvery time.Duration
	now        func() time.Time

	mu        sync.Mutex
	lastEval  map[string]time.Time // in-memory throttle state
	known     map[string]bool      // services whose throttle state was seeded from the store
	lastPrune time.Time
}

// New builds the ingestion service. Nil logger ⇒ slog's default.
func New(store Store, logger *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("instscoreingest.New: nil store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:      store,
		logger:     logger,
		window:     DefaultWindow,
		minReeval:  DefaultMinReeval,
		pruneEvery: DefaultPruneEvery,
		now:        time.Now,
		lastEval:   map[string]time.Time{},
		known:      map[string]bool{},
	}, nil
}

// IngestTraces folds one mirrored trace batch: split per service, persist the
// evidence rows, then (throttled) re-evaluate each touched service.
func (s *Service) IngestTraces(ctx context.Context, req *tracecolpb.ExportTraceServiceRequest) error {
	splits := SplitTraces(req)
	if len(splits) == 0 {
		return nil
	}
	now := s.now()
	rows := make([]WindowRow, 0, len(splits))
	names := make([]string, 0, len(splits))
	for _, sp := range splits {
		evidence, err := protojson.Marshal(sp.Request)
		if err != nil {
			return fmt.Errorf("instscoreingest.IngestTraces: marshal %q: %w", orUnnamed(sp.ServiceName), err)
		}
		rows = append(rows, WindowRow{
			ServiceName:   sp.ServiceName,
			Signal:        "traces",
			Evidence:      evidence,
			ReceivedAt:    now,
			SpanCount:     sp.Spans,
			ResourceCount: sp.Resources,
		})
		names = append(names, sp.ServiceName)
	}
	return s.commit(ctx, now, rows, names)
}

// IngestMetrics folds one mirrored metrics batch.
func (s *Service) IngestMetrics(ctx context.Context, req *metricscolpb.ExportMetricsServiceRequest) error {
	splits := SplitMetrics(req)
	if len(splits) == 0 {
		return nil
	}
	now := s.now()
	rows := make([]WindowRow, 0, len(splits))
	names := make([]string, 0, len(splits))
	for _, sp := range splits {
		evidence, err := protojson.Marshal(sp.Request)
		if err != nil {
			return fmt.Errorf("instscoreingest.IngestMetrics: marshal %q: %w", orUnnamed(sp.ServiceName), err)
		}
		rows = append(rows, WindowRow{
			ServiceName:   sp.ServiceName,
			Signal:        "metrics",
			Evidence:      evidence,
			ReceivedAt:    now,
			MetricCount:   sp.Metrics,
			ResourceCount: sp.Resources,
		})
		names = append(names, sp.ServiceName)
	}
	return s.commit(ctx, now, rows, names)
}

// IngestLogs folds one mirrored logs batch.
func (s *Service) IngestLogs(ctx context.Context, req *logscolpb.ExportLogsServiceRequest) error {
	splits := SplitLogs(req)
	if len(splits) == 0 {
		return nil
	}
	now := s.now()
	rows := make([]WindowRow, 0, len(splits))
	names := make([]string, 0, len(splits))
	for _, sp := range splits {
		evidence, err := protojson.Marshal(sp.Request)
		if err != nil {
			return fmt.Errorf("instscoreingest.IngestLogs: marshal %q: %w", orUnnamed(sp.ServiceName), err)
		}
		rows = append(rows, WindowRow{
			ServiceName:   sp.ServiceName,
			Signal:        "logs",
			Evidence:      evidence,
			ReceivedAt:    now,
			LogCount:      sp.Logs,
			ResourceCount: sp.Resources,
		})
		names = append(names, sp.ServiceName)
	}
	return s.commit(ctx, now, rows, names)
}

// commit persists the rows, opportunistically prunes outside-window evidence,
// then re-evaluates the touched services (throttled per service). Evaluation
// failure is logged, not returned: the rows are already durable and the next
// batch retries — the tap must not fail a batch over a report-only follow-up.
func (s *Service) commit(ctx context.Context, now time.Time, rows []WindowRow, names []string) error {
	if err := s.store.AppendWindow(ctx, rows); err != nil {
		return err
	}
	s.maybePrune(ctx, now)
	sort.Strings(names)
	for _, name := range names {
		if !s.due(name, now) {
			continue
		}
		if err := s.evaluate(ctx, name, now); err != nil {
			s.logger.Error("instscore: evaluation failed (will retry on a later batch)",
				"service", orUnnamed(name), "error", err.Error())
		}
	}
	return nil
}

// due reports whether the service's verdicts are stale enough to re-evaluate,
// seeding the throttle from the persisted verdict stamps on first sight after
// a process restart.
func (s *Service) due(name string, now time.Time) bool {
	s.mu.Lock()
	last, have := s.lastEval[name]
	seeded := s.known[name]
	s.mu.Unlock()
	if !seeded {
		s.mu.Lock()
		s.known[name] = true
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if at, ok, err := s.store.LatestEvaluatedAt(ctx, name); err == nil && ok {
			s.mu.Lock()
			s.lastEval[name] = at
			last, have = at, true
			s.mu.Unlock()
		}
	}
	if !have {
		return true
	}
	return now.Sub(last) >= s.minReeval
}

// evaluate re-folds the service's stored window through the SAME offline path
// the CLI uses (LoadDump → Accumulator → Evaluate), then upserts verdicts and
// the hourly score point. Evidence payloads are never logged.
func (s *Service) evaluate(ctx context.Context, name string, now time.Time) error {
	since := now.Add(-s.window)
	stored, err := s.store.LoadWindow(ctx, name, since)
	if err != nil {
		return fmt.Errorf("load window: %w", err)
	}
	doc, ok := buildDump(stored)
	if !ok {
		return nil // nothing in-window to score yet
	}
	acc, err := instscore.LoadDump(doc)
	if err != nil {
		return fmt.Errorf("re-fold window: %w", err)
	}
	window := acc.Window(name)
	if window == nil {
		return nil // rows exist but carry no records for this service
	}
	result := instscore.Evaluate(window)

	verdicts := make([]VerdictRow, 0, len(result.Verdicts))
	for _, v := range result.Verdicts {
		verdicts = append(verdicts, VerdictRow{
			RuleID:   v.Rule,
			Status:   string(v.Status),
			Evidence: v.Evidence,
			Impact:   ruleImpact[v.Rule],
		})
	}
	score := ScoreRow{
		ServiceName: name,
		BucketHour:  now.Truncate(time.Hour),
		Score:       result.Score,
		Category:    string(result.Category),
		Implemented: result.Implemented,
		Total:       result.Total,
		EvaluatedAt: now,
	}
	if err := s.store.SaveEvaluation(ctx, name, verdicts, score); err != nil {
		return fmt.Errorf("save evaluation: %w", err)
	}
	s.mu.Lock()
	s.lastEval[name] = now
	s.mu.Unlock()
	s.logger.Info("instscore: service evaluated",
		"service", orUnnamed(name), "score", result.Score,
		"category", string(result.Category), "rows", len(stored))
	return nil
}

// buildDump assembles the stored evidence rows into the multi-signal OTLP JSON
// dump shape LoadDump consumes. ok is false when there is nothing to fold.
func buildDump(rows []WindowRow) ([]byte, bool) {
	var traces, metrics, logs []json.RawMessage
	for _, r := range rows {
		switch r.Signal {
		case "traces":
			traces = append(traces, json.RawMessage(r.Evidence))
		case "metrics":
			metrics = append(metrics, json.RawMessage(r.Evidence))
		case "logs":
			logs = append(logs, json.RawMessage(r.Evidence))
		}
	}
	doc := map[string]any{}
	if len(traces) > 0 {
		doc["traces"] = traces
	}
	if len(metrics) > 0 {
		doc["metrics"] = metrics
	}
	if len(logs) > 0 {
		doc["logs"] = logs
	}
	if len(doc) == 0 {
		return nil, false
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return data, true
}

// maybePrune drops evidence outside the sliding window at most once per
// pruneEvery — cheap amortized retention, walked on the received_at index.
func (s *Service) maybePrune(ctx context.Context, now time.Time) {
	s.mu.Lock()
	last := s.lastPrune
	s.mu.Unlock()
	if now.Sub(last) < s.pruneEvery {
		return
	}
	s.mu.Lock()
	s.lastPrune = now
	s.mu.Unlock()
	pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := s.store.PruneBefore(pruneCtx, now.Add(-s.window))
	if err != nil {
		s.logger.Error("instscore: window prune failed", "error", err.Error())
		return
	}
	if n > 0 {
		s.logger.Info("instscore: pruned out-of-window evidence rows", "rows", n)
	}
}

func orUnnamed(name string) string {
	if name == "" {
		return "(no service.name)"
	}
	return name
}
