//go:build kpi

// Package buildbrowser_test contains the ISI-2169 KPI validation suite.
// Tests in this package assert the observability contracts defined in
// docs/bmad/design/build-browser-observability-kpis.md (ISI-2169).
//
// Run with: go test -tags kpi ./internal/observability/buildbrowser/...
// Do NOT add -tags kpi to the default CI gate; these run only in the Epic 8.7
// acceptance step, once ISI-2168 instrumentation lands.
package buildbrowser_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Minimal interfaces — the instrumentation layer must satisfy these.
// Wire in the real implementations when ISI-2168 stories close.
// ---------------------------------------------------------------------------

// Run is a completed run from the test corpus.
type Run struct {
	ID      uuid.UUID
	Status  string // "completed"
	Outcome string // "" | "failed"
}

// SnapshotArtifactStore is the artifact lookup the KPI assertion requires.
// The operator's artifact store (OBS-BB2 / 8.7c) must implement this.
type SnapshotArtifactStore interface {
	HasBuildSnapshot(ctx context.Context, runID uuid.UUID) (bool, error)
}

// SnapshotEmitMetrics reads emitted snapshot counters from the test metric sink.
// Wire to the OTel in-process metric reader used by the operator unit tests.
type SnapshotEmitMetrics interface {
	// EmitTotal returns the accumulated count for the given result label
	// ("ok" | "failed" | "skipped").
	EmitTotal(result string) int64
}

// CoverageAlertState is the alert evaluation result for a given run.
type CoverageAlertState int

const (
	AlertPending CoverageAlertState = iota
	AlertFiring
	AlertResolved
)

// CoverageAlertEvaluator evaluates the "no build view" SLO alert for one run.
// The alerting engine (Prometheus rule or equivalent) must implement this.
type CoverageAlertEvaluator interface {
	CoverageAlertState(runID uuid.UUID) CoverageAlertState
}

// TestCorpus is provided by the test fixture package (ISI-2168).
type TestCorpus interface {
	// CompletedSuccessRuns returns all runs with status=completed and outcome≠failed.
	CompletedSuccessRuns() []Run
}

// ---------------------------------------------------------------------------
// KPI-1 positive: every completed-success run has a build-snapshot artifact.
// ---------------------------------------------------------------------------

// TestBuildViewCoveragePositive asserts the SLO §4 join:
//
//	∀ run : run.status=completed AND run.outcome≠failed
//	  → EXISTS artifact WHERE artifact.run_id=run.id AND artifact.kind="build-snapshot"
//
// Pending: ISI-2168 8.7c (operator snapshot-emit instrumentation).
// Wire corpus and store before enabling.
func TestBuildViewCoveragePositive(t *testing.T) {
	t.Skip("pending ISI-2168 8.7c — wire corpus and artifact store")

	// Replace with real implementations from the Epic 8.7 test fixtures.
	var corpus TestCorpus            // = fixtures.LoadCorpus(t)
	var store SnapshotArtifactStore  // = artifacts.NewStore(db)
	ctx := context.Background()

	runs := corpus.CompletedSuccessRuns()
	if len(runs) == 0 {
		t.Fatal("test corpus contains no completed-success runs — cannot assert coverage SLO")
	}

	for _, run := range runs {
		has, err := store.HasBuildSnapshot(ctx, run.ID)
		if err != nil {
			t.Errorf("artifact store error for run %s: %v", run.ID, err)
			continue
		}
		if !has {
			t.Errorf("completed-success run %s has no build-snapshot artifact (SLO §4 violation)", run.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// KPI-1 negative: a forced-skip emit must trip the coverage alert.
//
// This is the failure mode a pure result=failed counter misses — the emit
// never ran, so no counter moved. Only the corpus join (run vs artifact) finds
// the gap.
// ---------------------------------------------------------------------------

// TestBuildViewCoverageNegativeSkipTripsAlert asserts that when snapshot-emit
// is forced to skip (not fail), the coverage alert still fires.
//
// Pending: ISI-2168 8.7c (operator) + alert rule wiring.
func TestBuildViewCoverageNegativeSkipTripsAlert(t *testing.T) {
	t.Skip("pending ISI-2168 8.7c — wire skip fixture, metrics sink, and alert evaluator")

	// Replace with real implementations.
	var metrics  SnapshotEmitMetrics     // = otel.NewTestSink()
	var store    SnapshotArtifactStore   // = artifacts.NewStore(db)
	var alertEv  CoverageAlertEvaluator  // = alerting.NewEvaluator(...)
	ctx := context.Background()

	// A run where the operator's snapshot-emit step is forced to skip (not fail).
	// Simulates a missed Collecting hook, operator restart, or skip-guard logic.
	var skippedRunID uuid.UUID // = fixtures.RunWithSkippedSnapshotEmit(t)

	// The skipped counter must have incremented so the alert has a signal to act on.
	if got := metrics.EmitTotal("skipped"); got != 1 {
		t.Errorf("snapshot.emit.total{result=skipped} = %d, want 1 — alert cannot fire on a silent counter", got)
	}
	// No failed counter must have moved (this is a skip, not a failure).
	if got := metrics.EmitTotal("failed"); got != 0 {
		t.Errorf("snapshot.emit.total{result=failed} = %d, want 0 — skip must not be counted as failure", got)
	}

	// The artifact join must find no build-snapshot (the emit was skipped).
	has, err := store.HasBuildSnapshot(ctx, skippedRunID)
	if err != nil {
		t.Fatalf("artifact store error: %v", err)
	}
	if has {
		t.Error("skipped run must not have a build-snapshot artifact")
	}

	// The coverage alert must be in a firing state (or would fire on next eval).
	state := alertEv.CoverageAlertState(skippedRunID)
	if state != AlertFiring {
		t.Errorf("coverage alert state = %v, want AlertFiring — skip must trip the coverage alert", state)
	}
}
