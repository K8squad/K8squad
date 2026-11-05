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

// runsummaryobs.go — per-completed-run summary-presence observability
// (ISI-7096, child of ISI-7088). ISI-7094 made a terminal run owe the thread
// EXACTLY ONE kind='summary' comment — the agent authors it, or the platform
// synthesizes one in the terminal txn (settle.go). This file is the regression
// GUARD for that invariant: every terminal settle emits a bounded metric pair
// recording whether the completed run actually carries a summary and how long
// it is, so a DT detector can fire the instant a completed run lands with ZERO
// summary comments (the whole feature regressing).
//
//	ksquad.run.summary.present{outcome,source,present}   counter    one per completed run; present="false" is the regression
//	ksquad.run.summary.length{outcome,source}            histogram ({char}) summary length distribution
//
// The guard is deliberately NOT trust-the-code: EmitRunSummaryObservation runs
// AFTER the terminal transaction commits and RE-QUERIES the committed
// coord.comment state. If a future change breaks the synthesize fallback, or a
// terminal run bypasses settle, the re-query finds no summary row and the
// metric reports present="false" / source="absent" — exactly what the detector
// alerts on. Reading the committed DB (not settle's in-function hasSummary) is
// the point: the signal verifies reality, not intent.
//
// Discipline mirrors phasespan.go EmitPhaseTransition — standalone,
// best-effort, called post-commit, so a telemetry failure can never roll back
// or fail a board move. The emitter is nop until a bootstrap site installs the
// OTel one (SetRunSummaryMetrics), so call sites never nil-check.
//
// Cardinality firewall (NFR-OBS3): every label is BOUNDED. outcome is the
// terminal-step enum (succeeded|failed|cancelled), source is the small
// authorship enum (agent|synthesized|absent), present is a bool. work_item id,
// run id and principal are NEVER metric labels — the per-run record lives on
// the coord.comment row and the audit_log, never on a metric series.
package coord

import (
	"context"
	"database/sql"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Instrument names (OTel dot-delimited; the Prometheus bridge renders these as
// ksquad_run_summary_present_total / ksquad_run_summary_length).
const (
	runSummaryPresentName = "ksquad.run.summary.present"
	runSummaryLengthName  = "ksquad.run.summary.length"
)

// Bounded attribute keys.
const (
	attrRunOutcome       = "outcome"
	attrRunSummarySource = "source"
	attrRunSummaryExists = "present"
)

// Summary-source enum — the bounded value set for the source label. It answers
// "who put the summary on the thread" and is derived post-commit from the
// summary row's author_principal (the engine principal ⇒ platform-synthesized;
// anyone else ⇒ the agent's own recap).
const (
	SummarySourceAgent       = "agent"       // the agent authored its own kind='summary' recap
	SummarySourceSynthesized = "synthesized" // the platform synthesized the required summary (settle fallback)
	SummarySourceAbsent      = "absent"      // NO summary on a completed run — the regression the detector alerts on
)

// RunSummaryObservation identifies one completed run to observe. It is a pure
// value; the caller has already committed the terminal transaction (summary
// row included) before emitting.
type RunSummaryObservation struct {
	// WorkItemID is the coord.work_item uuid the run settled. Required.
	WorkItemID string
	// RunID is the run uuid whose summary presence is being checked. Required
	// — summary presence is scoped per run (work_item + run_id), so an empty
	// run id skips emission (there is no per-run summary to verify).
	RunID string
	// TerminalStep is the reconcile step that settled (succeeded|failed|
	// cancelled). A non-terminal re-entry (a retry lap) owes no summary and is
	// skipped — matching settleIsTerminal.
	TerminalStep string
	// EnginePrincipal is the principal the platform synthesizes under. The
	// summary row's author_principal is compared against it to classify the
	// source as synthesized (engine) vs agent (anyone else).
	EnginePrincipal string
}

// RunSummaryMetrics is the summary-presence metric façade — the PhaseMetrics
// discipline so tests assert emissions with a fake and the signal set is named
// in exactly one place. Labels are BOUNDED enums, never per-item.
type RunSummaryMetrics interface {
	// RecordRunSummary records one completed-run observation: the counter
	// (always +1, keyed on outcome/source/present) and — when a summary
	// exists — the length histogram.
	RecordRunSummary(ctx context.Context, outcome, source string, present bool, length int)
	// Signals returns the names of every signal this emitter emits.
	Signals() []string
}

// nopRunSummaryMetrics is the default before a bootstrap site installs the OTel
// emitter. It still reports the full signal set (the contract is which signals
// EXIST).
type nopRunSummaryMetrics struct{}

func (nopRunSummaryMetrics) RecordRunSummary(context.Context, string, string, bool, int) {}
func (nopRunSummaryMetrics) Signals() []string {
	return []string{runSummaryPresentName, runSummaryLengthName}
}

// OTelRunSummaryMetrics implements RunSummaryMetrics on real OTel instruments
// registered on telemetry.Meter(). Mirrors scmmetrics: constructed at operator
// bootstrap AFTER telemetry.Setup so it binds the real meter, not the no-op.
type OTelRunSummaryMetrics struct {
	present metric.Int64Counter
	length  metric.Int64Histogram
}

// NewOTelRunSummaryMetrics registers the summary-presence instruments on meter.
// It returns an error only if the meter rejects an instrument name, which never
// happens for these static names.
func NewOTelRunSummaryMetrics(meter metric.Meter) (*OTelRunSummaryMetrics, error) {
	present, err := meter.Int64Counter(
		runSummaryPresentName,
		metric.WithDescription("Completed runs by terminal outcome, summary source and presence; present=\"false\" is a run that settled with no kind='summary' comment (ISI-7096 regression guard)."),
	)
	if err != nil {
		return nil, err
	}
	length, err := meter.Int64Histogram(
		runSummaryLengthName,
		metric.WithUnit("{char}"),
		metric.WithDescription("Per-completed-run summary length in characters, by outcome and source (ISI-7096)."),
	)
	if err != nil {
		return nil, err
	}
	return &OTelRunSummaryMetrics{present: present, length: length}, nil
}

func (m *OTelRunSummaryMetrics) RecordRunSummary(ctx context.Context, outcome, source string, present bool, length int) {
	presentStr := "false"
	if present {
		presentStr = "true"
	}
	m.present.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrRunOutcome, outcome),
		attribute.String(attrRunSummarySource, source),
		attribute.String(attrRunSummaryExists, presentStr),
	))
	// The length histogram records only for a run that HAS a summary — an
	// absent summary has no length to distribute, and a 0-length series would
	// drag the distribution down and hide the real regression (which the
	// present counter already carries).
	if present {
		m.length.Record(ctx, int64(length), metric.WithAttributes(
			attribute.String(attrRunOutcome, outcome),
			attribute.String(attrRunSummarySource, source),
		))
	}
}

func (m *OTelRunSummaryMetrics) Signals() []string {
	return []string{runSummaryPresentName, runSummaryLengthName}
}

var _ RunSummaryMetrics = nopRunSummaryMetrics{}
var _ RunSummaryMetrics = (*OTelRunSummaryMetrics)(nil)

// defaultRunSummaryMetrics is the process-wide emitter. It is nop until a
// bootstrap site installs the OTel one, mirroring phasespan's "safe before
// Setup" discipline so call sites never need a nil-check.
var defaultRunSummaryMetrics RunSummaryMetrics = nopRunSummaryMetrics{}

// SetRunSummaryMetrics installs the process summary-presence emitter. Called
// once at bootstrap (cmd/operator), after telemetry.Setup; not safe for
// concurrent use with EmitRunSummaryObservation (bootstrap happens before
// serving). A nil emitter is ignored.
func SetRunSummaryMetrics(m RunSummaryMetrics) {
	if m != nil {
		defaultRunSummaryMetrics = m
	}
}

// summaryQuerier is the minimal read surface EmitRunSummaryObservation needs —
// satisfied by *sql.DB (the committed process handle both terminal callers
// hold). Taking an interface keeps the emitter unit-testable with a fake.
type summaryQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EmitRunSummaryObservation records the summary-presence metric for one
// completed run. It is best-effort and never returns an error: the caller has
// already committed the terminal settle, and observability must not be able to
// fail a board move. Safe to call before telemetry.Setup (metrics nop).
//
// The guard re-queries the COMMITTED coord.comment state rather than trusting
// settle's in-function bookkeeping: present/source/length are read from the
// summary row that actually landed, so a broken synthesize path or a run that
// skipped settle reports present=false / source=absent and trips the detector.
//
// Gating mirrors settle: a non-terminal step, an empty work-item/run id, or a
// nil db is a no-op (a retry lap owes no summary; there is nothing to verify).
func EmitRunSummaryObservation(ctx context.Context, db summaryQuerier, ev RunSummaryObservation) {
	if db == nil || ev.WorkItemID == "" || ev.RunID == "" || !settleIsTerminal(ev.TerminalStep) {
		return
	}

	var body, author sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT body, author_principal
		  FROM coord.comment
		 WHERE work_item_id = $1::uuid AND run_id = $2::uuid AND kind = 'summary'
		 ORDER BY created_at ASC
		 LIMIT 1`,
		ev.WorkItemID, ev.RunID).Scan(&body, &author)

	switch err {
	case nil:
		// A summary landed. Classify its source by authorship: the platform
		// synthesizes under the engine principal (settle.go), so a summary row
		// authored by the engine principal is synthesized; anyone else is the
		// agent's own recap.
		source := SummarySourceAgent
		if author.Valid && ev.EnginePrincipal != "" && author.String == ev.EnginePrincipal {
			source = SummarySourceSynthesized
		}
		defaultRunSummaryMetrics.RecordRunSummary(ctx, ev.TerminalStep, source, true, utf8.RuneCountInString(body.String))
	case sql.ErrNoRows:
		// The regression: a completed run with ZERO summary comments. This is
		// what the missing-summary detector alerts on.
		defaultRunSummaryMetrics.RecordRunSummary(ctx, ev.TerminalStep, SummarySourceAbsent, false, 0)
	default:
		// A query hiccup degrades to silence rather than a phantom reading —
		// the board move already committed; we simply emit nothing for this run.
		return
	}
}
