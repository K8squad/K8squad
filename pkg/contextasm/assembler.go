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

package contextasm

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

// ============================================================================
// Story 3.6 — the Context Assembler
// ============================================================================
//
// Lives in the Run reconciler's Claiming → Running transition (§8): gathers
// the five content classes — work item (description/AC/comments), project
// metadata (repo/ref, arch-doc refs, conventions), goals (Project CRD +
// work-item), scoped memory recall (6.6), linked artifacts (5.4 mirror) —
// ASSEMBLED BY THE CONTROL PLANE, NEVER THE AGENT, then budgets (5.9) and
// snapshots the resolved inputs on the Run for audit + re-entrant reuse.

// WorkItemFacts is the fenced coordination-record read for one work item
// (§6): the authoritative task content plus its revision token.
type WorkItemFacts struct {
	ID                 string
	Revision           string // coordination-DB revision (opaque, ADR-001)
	Title              string
	Description        string
	AcceptanceCriteria []string
	Goals              []string // work-item-level goals
	Comments           []Comment
}

// Comment is a work-item comment (authoritative tier: part of the §8.5
// work-item "comment history").
type Comment struct {
	Author    string
	Content   string
	WrittenAt string
}

// ProjectMeta is the project-metadata content class (§8.5): repo URL/ref,
// arch-doc refs, conventions. Sourced from the Project CRD + config.
type ProjectMeta struct {
	ProjectRevision string // Project metadata.generation — the goal revision
	RepoURL         string
	RepoRef         string
	ArchDocRefs     []string
	Conventions     string
	Goals           []string // Project CRD goals (§5.1)
}

// RecallDoc is one scoped memory-recall result, already projected through
// the §7.3 untrusted envelope by the memory service (6.6): {content, author,
// written_at, scope, trust:"untrusted"} + the record id for the snapshot.
type RecallDoc struct {
	ID        string
	Content   string
	Author    string
	Scope     string
	WrittenAt string
	Score     float64 // relevance (distance-derived); higher = keep longer
}

// ArtifactLink is one linked artifact from the §5.4 SCM mirror / build
// outputs: reference material, untrusted-external.
type ArtifactLink struct {
	URI    string
	Digest string
	Kind   string // e.g. "pr", "buildOutput", "release"
	Body   string // mirrored content excerpt (data, never instructions)
}

// Sources is the control-plane gather seam for the five §8.5 content
// classes. The Run reconciler wires production readers (apiserver/coord
// store, Project CRD, memory service 6.6, SCM mirror 5.4); tests fake it.
// The pinned arguments express re-entrant determinism: with a snapshot, the
// assembler re-reads EXACTLY the pinned revision/doc ids, never latest.
type Sources interface {
	// WorkItem reads the work item. rev=="" reads latest; otherwise the
	// pinned revision (snapshot reuse, §6.4) — a rev mismatch must error,
	// never silently fall back to latest.
	WorkItem(ctx context.Context, id, rev string) (WorkItemFacts, error)
	// ProjectMeta reads the Project's metadata + goals. revision=="" reads
	// current; otherwise the pinned generation (snapshot reuse).
	ProjectMeta(ctx context.Context, projectRef, revision string) (ProjectMeta, error)
	// MemoryRecall runs the scoped §7 semantic recall (6.6) — project/squad
	// scope, untrusted tier. ids pins the exact doc set (snapshot reuse);
	// empty does a fresh relevance query.
	MemoryRecall(ctx context.Context, teamID string, projectID string, ids []string, topK int) ([]RecallDoc, error)
	// Artifacts lists the Run's linked artifacts (§5.4 mirror / §6.1
	// artifact rows).
	Artifacts(ctx context.Context, runID string) ([]ArtifactLink, error)
}

// Assembler builds §8.5 envelopes. Construct with NewAssembler; the zero
// value is not usable.
type Assembler struct {
	sources Sources
	// TopK bounds the fresh memory recall when no snapshot pins the doc set.
	TopK int
}

// NewAssembler returns an Assembler reading sources. topK<=0 defaults to 8.
func NewAssembler(sources Sources, topK int) *Assembler {
	if topK <= 0 {
		topK = 8
	}
	return &Assembler{sources: sources, TopK: topK}
}

// AssembleRequest is one Run's assembly input: the CRD trio plus the
// resolved model window (§10.1 Agent Card contextWindow, model-keyed) and —
// on re-entrant resume — the Run's existing snapshot to reuse (§6.4).
type AssembleRequest struct {
	Run           *ksquadv1alpha1.Run
	Agent         *ksquadv1alpha1.Agent
	Project       *ksquadv1alpha1.Project
	TeamID        string
	ContextWindow int64
	Existing      *ksquadv1alpha1.ContextSnapshot
}

// AssembleResult is the assembled envelope, the budget actually applied, the
// Run-status snapshot to persist, and the shim injection payload.
type AssembleResult struct {
	Envelope  *Envelope
	Budget    Budget
	Snapshot  *ksquadv1alpha1.ContextSnapshot
	Injection *InjectionPayload
}

// Assemble runs the full 3.6+5.9 pipeline for one Claiming → Running
// transition:
//
//  1. gather (or re-read pinned, when req.Existing is set — the resumed Run
//     sees identical context, §6.4);
//  2. tier-stamp into an envelope (server-side constants, F16);
//  3. resolve the budget Project → Agent → clamp-by-window (fail-closed on
//     an over-window tier);
//  4. apply the priority-ordered budget (fail-closed when must-include alone
//     exceeds the window);
//  5. snapshot the resolved inputs for the Run status;
//  6. render the shim injection payload (tiers preserved, 5.9).
func (a *Assembler) Assemble(ctx context.Context, req AssembleRequest) (_ *AssembleResult, err error) {
	// AC7 / o11y contract (ISI-3592 §2, §9 steps 1-3): the whole assembly is
	// one `contextasm.assemble` span, a child of the caller's `run.reconcile`
	// span via ctx. The named err return lets one deferred hook stamp failure
	// across every return path. Only counts/ids/sizes/revisions are emitted —
	// NEVER element .Content: the bootstrap path is the highest-PII surface
	// (§8), so telemetry here must not materialize work-item, comment or recall
	// text.
	ctx, span := telemetry.Tracer().Start(ctx, "contextasm.assemble")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			// ErrMustIncludeExceedsWindow is the load-bearing fail-closed path
			// (story 5.9): always mark it so it is observable, never silent.
			if errors.Is(err, ErrMustIncludeExceedsWindow) {
				span.SetAttributes(attribute.Bool("ksquad.contextasm.fail_closed", true))
			}
		}
		span.End()
	}()

	if req.Run == nil || req.Agent == nil || req.Project == nil {
		return nil, fmt.Errorf("contextasm: run, agent and project are required")
	}
	if req.Run.Spec.WorkItemRef == "" {
		return nil, fmt.Errorf("contextasm: run.spec.workItemRef is required")
	}
	if req.ContextWindow <= 0 {
		return nil, fmt.Errorf("contextasm: resolved contextWindow is required (model-keyed Agent Card capability, §10.1)")
	}

	// Identity attributes (§2.1) — ids/refs/window/resume, no free-form text.
	span.SetAttributes(
		attribute.String("ksquad.run.id", string(req.Run.UID)),
		attribute.String("ksquad.run.work_item_ref", req.Run.Spec.WorkItemRef),
		attribute.String("ksquad.project", req.Run.Spec.ProjectRef.Name),
		attribute.String("ksquad.team", req.TeamID),
		attribute.Bool("ksquad.contextasm.resume", req.Existing != nil),
		attribute.Int64("ksquad.contextasm.context_window", req.ContextWindow),
	)

	pinnedWiRev, pinnedGoalRev, pinnedDocIDs := "", "", []string(nil)
	if req.Existing != nil {
		pinnedWiRev = req.Existing.WorkItemRevision
		pinnedGoalRev = req.Existing.GoalRevision
		pinnedDocIDs = req.Existing.MemoryDocIDs
	}

	wi, err := a.gatherWorkItem(ctx, req.Run.Spec.WorkItemRef, pinnedWiRev)
	if err != nil {
		return nil, fmt.Errorf("contextasm: work item %q: %w", req.Run.Spec.WorkItemRef, err)
	}
	meta, err := a.gatherProjectMeta(ctx, req.Run.Spec.ProjectRef.Name, pinnedGoalRev)
	if err != nil {
		return nil, fmt.Errorf("contextasm: project meta: %w", err)
	}
	recall, err := a.gatherMemoryRecall(ctx, req.TeamID, req.Run.Spec.ProjectRef.Name, pinnedDocIDs)
	if err != nil {
		return nil, fmt.Errorf("contextasm: memory recall: %w", err)
	}
	arts, err := a.gatherArtifacts(ctx, string(req.Run.UID))
	if err != nil {
		return nil, fmt.Errorf("contextasm: artifacts: %w", err)
	}

	env := a.buildEnvelope(wi, meta, recall, arts, req.Run.Spec.Inputs)

	// Deterministic resume (AC3): when resuming, reuse the budget the snapshot
	// pinned rather than re-resolving from the live Project/Agent. Combined
	// with the caller pinning ContextWindow off the snapshot, this makes the
	// resumed envelope byte-identical even if spec.model / contextBudgetOverride
	// changed after the snapshot was stored. A fresh drive resolves normally.
	var budget Budget
	if req.Existing != nil && req.Existing.Budget != nil {
		budget = budgetFromSnapshot(req.Existing.Budget)
		span.AddEvent("contextasm.budget.resumed")
	} else {
		budget, err = ResolveBudget(projectBudget(req.Project), agentBudget(req.Agent), req.ContextWindow)
		if err != nil {
			return nil, err
		}
		span.AddEvent("contextasm.budget.resolved")
	}

	injection := NewInjection(env)
	budgeted, err := ApplyBudget(env, budget, req.ContextWindow, injection.OverheadTokens())
	if err != nil {
		return nil, err
	}

	stats := envelopeTelemetryStats(budgeted)
	span.AddEvent("contextasm.budget.applied", trace.WithAttributes(
		attribute.Int("ksquad.contextasm.dropped_elements", stats.dropped),
		attribute.StringSlice("ksquad.contextasm.truncated_tiers", stats.truncatedTiers),
	))

	snapshot := a.buildSnapshot(wi, meta, budget, req.ContextWindow)
	pinRecallIDs(snapshot, recall, budgeted)
	span.AddEvent("contextasm.snapshot.written")
	finalInjection := NewInjection(budgeted)

	// Outcome attributes (§2.1) — element counts per tier, budgeted tokens,
	// truncation, recall accounting, and pinned revisions. Sizes and ids only.
	span.SetAttributes(
		attribute.Int("ksquad.contextasm.elements.authoritative", stats.authoritative),
		attribute.Int("ksquad.contextasm.elements.untrusted_recall", stats.untrustedRecall),
		attribute.Int("ksquad.contextasm.elements.untrusted_external", stats.untrustedExternal),
		attribute.Int64("ksquad.contextasm.tokens.work_item", budget.WorkItem),
		attribute.Int64("ksquad.contextasm.tokens.project_docs", budget.ProjectDocs),
		attribute.Int64("ksquad.contextasm.tokens.memory_recall", budget.MemoryRecall),
		attribute.Int64("ksquad.contextasm.tokens.artifacts", budget.Artifacts),
		attribute.StringSlice("ksquad.contextasm.truncated_tiers", stats.truncatedTiers),
		attribute.Int("ksquad.contextasm.dropped_elements", stats.dropped),
		attribute.Int("ksquad.contextasm.recall_docs.returned", len(recall)),
		attribute.Int("ksquad.contextasm.recall_docs.kept", len(snapshot.MemoryDocIDs)),
		attribute.String("ksquad.contextasm.snapshot.work_item_revision", snapshot.WorkItemRevision),
		attribute.String("ksquad.contextasm.snapshot.goal_revision", snapshot.GoalRevision),
	)

	return &AssembleResult{
		Envelope:  budgeted,
		Budget:    budget,
		Snapshot:  snapshot,
		Injection: finalInjection,
	}, nil
}

// gatherWorkItem / gatherProjectMeta / gatherMemoryRecall / gatherArtifacts
// wrap each of the four real latency/failure points (DB, CRD, memory service,
// SCM mirror) in a `contextasm.source.*` child span (o11y §2.2). A pinned
// read (resume) sets ksquad.contextasm.pinned=true; a pinned-revision mismatch
// (deterministic-resume guard) surfaces as an error span, never a silent
// fallback. result_count is the collection size returned (0 for scalar reads).
func (a *Assembler) gatherWorkItem(ctx context.Context, id, rev string) (WorkItemFacts, error) {
	ctx, span := startSourceSpan(ctx, "work_item", rev != "")
	wi, err := a.sources.WorkItem(ctx, id, rev)
	endSourceSpan(span, len(wi.Comments), err)
	return wi, err
}

func (a *Assembler) gatherProjectMeta(ctx context.Context, projectRef, revision string) (ProjectMeta, error) {
	ctx, span := startSourceSpan(ctx, "project_meta", revision != "")
	meta, err := a.sources.ProjectMeta(ctx, projectRef, revision)
	endSourceSpan(span, 0, err) // single CRD read (scalar)
	return meta, err
}

func (a *Assembler) gatherMemoryRecall(ctx context.Context, teamID, projectID string, ids []string) ([]RecallDoc, error) {
	ctx, span := startSourceSpan(ctx, "memory_recall", len(ids) > 0)
	recall, err := a.sources.MemoryRecall(ctx, teamID, projectID, ids, a.TopK)
	endSourceSpan(span, len(recall), err)
	return recall, err
}

func (a *Assembler) gatherArtifacts(ctx context.Context, runID string) ([]ArtifactLink, error) {
	ctx, span := startSourceSpan(ctx, "artifacts", false)
	arts, err := a.sources.Artifacts(ctx, runID)
	endSourceSpan(span, len(arts), err)
	return arts, err
}

func startSourceSpan(ctx context.Context, source string, pinned bool) (context.Context, trace.Span) {
	return telemetry.Tracer().Start(ctx, "contextasm.source."+source, trace.WithAttributes(
		attribute.String("ksquad.contextasm.source", source),
		attribute.Bool("ksquad.contextasm.pinned", pinned),
	))
}

func endSourceSpan(span trace.Span, resultCount int, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Int("ksquad.contextasm.result_count", resultCount))
	}
	span.End()
}

// assembleStats are the PII-safe counts derived from the budgeted envelope for
// the assemble span (§2.1). It reads el.Content ONLY to compare against the
// constant truncateMarker — it never emits content.
type assembleStats struct {
	authoritative     int
	untrustedRecall   int
	untrustedExternal int
	dropped           int      // elements fully dropped by the budgeter
	truncatedTiers    []string // distinct tiers with a truncated element
}

func envelopeTelemetryStats(env *Envelope) assembleStats {
	var s assembleStats
	seen := map[TrustTier]bool{}
	for _, el := range env.Elements {
		switch el.Tier {
		case TierAuthoritative:
			s.authoritative++
		case TierUntrustedRecall:
			s.untrustedRecall++
		case TierUntrustedExternal:
			s.untrustedExternal++
		}
		if el.Content == truncateMarker {
			s.dropped++
		}
		if el.Truncated && !seen[el.Tier] {
			seen[el.Tier] = true
			s.truncatedTiers = append(s.truncatedTiers, string(el.Tier))
		}
	}
	return s
}

// buildEnvelope tier-stamps the gathered facts (the ONLY envelope
// construction path — server-side constants by source, F16).
func (a *Assembler) buildEnvelope(wi WorkItemFacts, meta ProjectMeta, recall []RecallDoc, arts []ArtifactLink, inputs map[string]string) *Envelope {
	b := newEnvelopeBuilder()

	// — Authoritative: the task itself (must-include, 5.9) —
	b.addAuthoritative("description", joinTitleBody(wi.Title, wi.Description), Provenance{Source: "workItem"})
	for i, ac := range wi.AcceptanceCriteria {
		b.addAuthoritative("acceptanceCriteria", ac, Provenance{Source: "acceptanceCriteria", Author: fmt.Sprintf("index:%d", i)})
	}
	for _, g := range append(meta.Goals, wi.Goals...) { // Project goals then work-item goals
		b.addAuthoritative("goal", g, Provenance{Source: "goals"})
	}
	for _, c := range wi.Comments {
		b.addAuthoritative("comment", c.Content, Provenance{Source: "workItem", Author: c.Author, WrittenAt: c.WrittenAt})
	}

	// — Authoritative tier, best-effort class: project metadata (5.4) —
	if meta.RepoURL != "" {
		ref := meta.RepoRef
		if ref == "" {
			ref = "default"
		}
		b.addProjectMeta("repo", meta.RepoURL+" @"+ref, Provenance{Source: "projectMeta"})
	}
	for _, d := range meta.ArchDocRefs {
		b.addProjectMeta("archDoc", d, Provenance{Source: "projectMeta"})
	}
	if meta.Conventions != "" {
		b.addProjectMeta("conventions", meta.Conventions, Provenance{Source: "projectMeta"})
	}
	for _, kv := range sortedInputs(inputs) {
		b.addProjectMeta("input", kv.k+"="+kv.v, Provenance{Source: "runInputs"})
	}

	// — Untrusted-recall: memory (§7.3 shape, reference never commands) —
	for _, r := range recall {
		b.addUntrustedRecall("recall", r.Content, Provenance{
			Source:    "memory",
			Author:    r.Author,
			WrittenAt: r.WrittenAt,
			Scope:     r.Scope,
		}, r.Score)
	}

	// — Untrusted-external: synced repo/PR/artifact content (D8) —
	for _, art := range arts {
		digest := art.Digest
		if digest == "" {
			digest = "-"
		}
		b.addUntrustedExternal(art.Kind, art.URI+" ("+digest+")\n"+art.Body, Provenance{Source: "artifact"})
	}

	return b.build()
}

// buildSnapshot pins the resolved inputs (§6.4/§8.5): what the agent
// actually saw, for audit + deterministic resume.
func (a *Assembler) buildSnapshot(wi WorkItemFacts, meta ProjectMeta, budget Budget, window int64) *ksquadv1alpha1.ContextSnapshot {
	snap := &ksquadv1alpha1.ContextSnapshot{
		WorkItemRevision: wi.Revision,
		GoalRevision:     meta.ProjectRevision,
		ContextWindow:    &window,
	}
	budgetCopy := ksquadv1alpha1.ContextBudget{
		WorkItem:     i64ptr(budget.WorkItem),
		ProjectDocs:  i64ptr(budget.ProjectDocs),
		MemoryRecall: i64ptr(budget.MemoryRecall),
		Artifacts:    i64ptr(budget.Artifacts),
	}
	snap.Budget = &budgetCopy
	return snap
}

// pinRecallIDs records on the snapshot the recall doc ids (relevance order)
// that survived the budget — the exact memory set the agent saw (§6.4 audit:
// "what did the agent actually see?").
func pinRecallIDs(snap *ksquadv1alpha1.ContextSnapshot, recall []RecallDoc, env *Envelope) {
	kept := map[string]bool{}
	for _, el := range env.Elements {
		if el.Tier == TierUntrustedRecall && el.Content != truncateMarker {
			kept[el.Content] = true
		}
	}
	ids := make([]string, 0, len(recall))
	for _, r := range recall {
		if kept[r.Content] {
			ids = append(ids, r.ID)
		}
	}
	snap.MemoryDocIDs = ids
}

func i64ptr(v int64) *int64 { return &v }

// budgetFromSnapshot rebuilds the applied Budget from a pinned snapshot for
// deterministic resume — the exact per-tier allocation the first drive
// recorded, so re-assembly reproduces identical bytes.
func budgetFromSnapshot(b *ksquadv1alpha1.ContextBudget) Budget {
	return Budget{
		WorkItem:     derefI64(b.WorkItem),
		ProjectDocs:  derefI64(b.ProjectDocs),
		MemoryRecall: derefI64(b.MemoryRecall),
		Artifacts:    derefI64(b.Artifacts),
	}
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func joinTitleBody(title, body string) string {
	if title == "" {
		return body
	}
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

func sortedInputs(m map[string]string) []struct{ k, v string } {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]struct{ k, v string }, 0, len(keys))
	for _, k := range keys {
		out = append(out, struct{ k, v string }{k, m[k]})
	}
	return out
}

func projectBudget(p *ksquadv1alpha1.Project) *ksquadv1alpha1.ContextBudget {
	if p == nil {
		return nil
	}
	return p.Spec.ContextBudget
}

func agentBudget(a *ksquadv1alpha1.Agent) *ksquadv1alpha1.ContextBudget {
	if a == nil {
		return nil
	}
	return a.Spec.ContextBudgetOverride
}
