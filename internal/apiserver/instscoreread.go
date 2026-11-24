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

package apiserver

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/instscoreingest"
	"github.com/K8squad/K8squad/pkg/telemetry/instscore"
)

// ============================================================================
// ISI-7750 — instrumentation-score READ surface (phase 4 of the ISI-7741
// plan): GET /api/v1/instscore/services and GET /api/v1/instscore/services/
// {name}. Score, category, rule-by-rule verdicts with impact + failed-rule
// guidance, fed by the phase-3 ingestion store (migration 0042).
// ============================================================================
//
// This is the report half of the v1 deliverable ("report-only, no gates, no
// alerts"). It rides the SAME §13 BFF authz choke point as the other read
// models — the console session is the identity, and the data is the fleet's
// own workload telemetry scoring (the OTel gateway tap of ISI-7749 wrote every
// row), so there is no per-tenant scoping to apply: every authenticated caller
// of a deployment sees that deployment's services. The ingest surface it reads
// from stays on its own bearer auth at /api/instscore/v1/*; the two never mix.
//
// Enrichment (rule target, description, failed-rule guidance) is stamped here
// from the pinned engine registry + guidance maps (pkg/telemetry/instscore),
// never stored: the DB holds vendor-neutral verdicts, and the API adds the
// vendored-standard context on the way out. A rule the registry knows but the
// store has no verdict for (the last evaluation predates a registry update)
// surfaces as not_evaluated with an explicit "no stored verdict" evidence —
// the disclosure direction the spec mandates, never a silent pass.

// InstScoreServiceSummary is one services-list row: the JSON shape of
// instscoreingest.ServiceSummary.
type InstScoreServiceSummary struct {
	Service       string     `json:"service"`
	Score         *float64   `json:"score,omitempty"`
	Category     string     `json:"category,omitempty"`
	Implemented  int        `json:"implemented"`
	Total        int        `json:"total"`
	EvaluatedAt  *time.Time `json:"evaluated_at,omitempty"`
	Pass         int        `json:"pass"`
	Fail         int        `json:"fail"`
	NotEvaluated int        `json:"not_evaluated"`
	Spans        int        `json:"spans"`
	Logs         int        `json:"logs"`
	Metrics      int        `json:"metrics"`
	Resources    int        `json:"resources"`
	FirstBatchAt *time.Time `json:"first_batch_at,omitempty"`
	LastBatchAt  *time.Time `json:"last_batch_at,omitempty"`
}

// InstScoreVerdict is one rule's verdict enriched with the vendored-standard
// context: what the rule is (target/impact/description), what it scored
// (status/evidence), and — the failed-rule guidance the plan calls for — the
// vendored Criteria spelling out what MUST hold.
type InstScoreVerdict struct {
	Rule        string    `json:"rule"`
	Target      []string  `json:"target,omitempty"`
	Impact      string    `json:"impact"`
	Status      string    `json:"status"`
	Evidence    string    `json:"evidence"`
	Description string    `json:"description,omitempty"`
	Guidance    string    `json:"guidance,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// InstScoreHistoryPoint is one hourly score sample, oldest first.
type InstScoreHistoryPoint struct {
	BucketHour time.Time `json:"bucket_hour"`
	Score      float64   `json:"score"`
}

// InstScoreServiceDetail is the per-service payload: the list row plus the
// full rule-by-rule verdict table and the hourly history tail.
type InstScoreServiceDetail struct {
	InstScoreServiceSummary
	Verdicts []InstScoreVerdict      `json:"verdicts"`
	History  []InstScoreHistoryPoint `json:"history"`
}

// InstScoreServicesResponse is the list payload. RulesTotal is the pinned
// registry size — the denominator of the spec's implemented/total disclosure.
type InstScoreServicesResponse struct {
	Services   []InstScoreServiceSummary `json:"services"`
	RulesTotal int                       `json:"rules_total"`
}

// InstScoreReader is the read seam behind both routes. Production wires the
// instscoreingest SQLStore (the same handle the ingestion tap writes through);
// tests wire a fake. ListServices ordering (worst score first, pending last)
// and the detail's rule-ID verdict order come from the store; the handlers
// never re-sort.
type InstScoreReader interface {
	ListServices(ctx context.Context) ([]instscoreingest.ServiceSummary, error)
	GetService(ctx context.Context, serviceName string) (instscoreingest.ServiceDetail, bool, error)
}

// instscoreServices is the handler behind GET /api/v1/instscore/services. It
// rides the §13 BFF authz choke point; a caller with no resolved principal
// gets 401 (defence in depth over BFFAuthz), a reader error is 502. An empty
// deployment is a normal empty list, not an error — nothing has tapped
// telemetry yet.
func (s *Server) instscoreServices(reader InstScoreReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth, ok := discussion.AuthFromContext(r.Context()); !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		summaries, err := reader.ListServices(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "instscore read model unavailable")
			return
		}
		resp := InstScoreServicesResponse{
			Services:   make([]InstScoreServiceSummary, 0, len(summaries)),
			RulesTotal: len(instscore.Rules),
		}
		for _, sm := range summaries {
			resp.Services = append(resp.Services, instScoreSummaryJSON(sm))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// instscoreServiceDetail is the handler behind GET /api/v1/instscore/services/
// {name}. Unknown service → 404 (the name is caller-chosen, so absence is a
// plain not-found, unlike the list's no-404 tenancy discipline). {name} cannot
// be empty (mux needs a path segment), so the unnamed service.name bucket —
// which the LIST keeps visible for its RES-005 violation evidence — is
// list-only by construction; the console renders it non-navigable.
func (s *Server) instscoreServiceDetail(reader InstScoreReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth, ok := discussion.AuthFromContext(r.Context()); !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		name := muxVar(r, "name")
		detail, found, err := reader.GetService(r.Context(), name)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "instscore read model unavailable")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "service not observed in the instrumentation-score window")
			return
		}
		writeJSON(w, http.StatusOK, instScoreDetailJSON(detail))
	}
}

// instScoreSummaryJSON maps a store row to its JSON shape.
func instScoreSummaryJSON(sm instscoreingest.ServiceSummary) InstScoreServiceSummary {
	return InstScoreServiceSummary{
		Service:      sm.ServiceName,
		Score:        sm.Score,
		Category:     sm.Category,
		Implemented:  sm.Implemented,
		Total:        sm.Total,
		EvaluatedAt:  sm.EvaluatedAt,
		Pass:         sm.PassCount,
		Fail:         sm.FailCount,
		NotEvaluated: sm.SkipCount,
		Spans:        sm.SpanCount,
		Logs:         sm.LogCount,
		Metrics:      sm.MetricCount,
		Resources:    sm.ResourceCount,
		FirstBatchAt: sm.FirstBatchAt,
		LastBatchAt:  sm.LastBatchAt,
	}
}

// instScoreDetailJSON builds the detail payload: every stored verdict
// enriched from the registry, then every registry rule with NO stored verdict
// appended as not_evaluated ("registry newer than the last evaluation"), the
// union sorted by rule ID for a stable render.
func instScoreDetailJSON(d instscoreingest.ServiceDetail) InstScoreServiceDetail {
	out := InstScoreServiceDetail{
		InstScoreServiceSummary: instScoreSummaryJSON(d.Summary),
		Verdicts:                make([]InstScoreVerdict, 0, len(instscore.Rules)),
		History:                 make([]InstScoreHistoryPoint, 0, len(d.History)),
	}
	seen := make(map[string]bool, len(d.Verdicts))
	for _, v := range d.Verdicts {
		jv := InstScoreVerdict{
			Rule:        v.RuleID,
			Impact:      v.Impact,
			Status:      v.Status,
			Evidence:    v.Evidence,
			EvaluatedAt: v.EvaluatedAt,
		}
		if rule, ok := instscore.ByID(v.RuleID); ok {
			jv.Target = targetsJSON(rule)
			jv.Impact = string(rule.Impact) // the registry is authoritative
			if desc, ok := instscore.Description(v.RuleID); ok {
				jv.Description = desc
			}
		}
		// Guidance is the failed-rule remediation text: attach it for failed
		// rules (and for not_evaluated, where the exclusion reason is the
		// evidence) so the console can always render "what to fix".
		if v.Status == string(instscore.StatusFail) || v.Status == string(instscore.StatusNotEvaluated) {
			if g, ok := instscore.FailedGuidance(v.RuleID); ok {
				jv.Guidance = g
			}
		}
		seen[v.RuleID] = true
		out.Verdicts = append(out.Verdicts, jv)
	}
	for _, rule := range instscore.Rules {
		if seen[rule.ID] {
			continue
		}
		jv := InstScoreVerdict{
			Rule:        rule.ID,
			Target:      targetsJSON(rule),
			Impact:      string(rule.Impact),
			Status:      string(instscore.StatusNotEvaluated),
			Evidence:    "no stored verdict — the rule registry is newer than this service's last evaluation",
			EvaluatedAt: time.Time{},
		}
		if desc, ok := instscore.Description(rule.ID); ok {
			jv.Description = desc
		}
		if g, ok := instscore.FailedGuidance(rule.ID); ok {
			jv.Guidance = g
		}
		out.Verdicts = append(out.Verdicts, jv)
	}
	sort.Slice(out.Verdicts, func(i, j int) bool { return out.Verdicts[i].Rule < out.Verdicts[j].Rule })
	for _, p := range d.History {
		out.History = append(out.History, InstScoreHistoryPoint{BucketHour: p.BucketHour, Score: p.Score})
	}
	return out
}

// targetsJSON renders a rule's target signals as JSON strings.
func targetsJSON(rule instscore.Rule) []string {
	out := make([]string, 0, len(rule.Target))
	for _, t := range rule.Target {
		out = append(out, string(t))
	}
	return out
}
