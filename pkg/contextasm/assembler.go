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
	"strings"

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

	// Discussion, when non-nil, carries the room context for a work item that
	// was minted from an @-mention in a discussion thread (source='discussion',
	// ISI-5108 / mentiondispatch). The work item's own Title/Description for such
	// a run is a STATIC "go read the thread, reply in the room" instruction — not
	// the real conversation topic — so the production Sources populates this with
	// the actual transcript + roster + triggering message. The assembler injects
	// the transcript UNTRUSTED (cross-participant content, prompt-injection
	// guardrail) and keys semantic recall off the real topic (ISI-5275).
	// Nil for an ordinary board run.
	Discussion *DiscussionContext
}

// DiscussionContext is the §8.5 content for a discussion-room run (ISI-5275,
// parent ISI-5270 WS-A). It is cross-participant conversation content: the
// transcript rides the UNTRUSTED-external tier (a room participant is not a
// trusted KSquad principal — their words are data, never commands, exactly like
// the memory/artifact tiers), while the roster (server-derived participant
// names) rides the best-effort project-meta class. The triggering message is
// carried separately so recall can key off the real topic, not the boilerplate
// instruction in the work-item body.
type DiscussionContext struct {
	// ThreadID is the room thread the run must read + reply into (for legibility
	// in the assembled context; the reply path is already in the instruction body).
	ThreadID string
	// TriggerMessage is the body of the @-mention that triggered the run — the
	// single most topic-bearing line, weighted first in the recall query.
	TriggerMessage string
	// Transcript is the bounded, chronological (oldest→newest) tail of the thread
	// — last N messages, already capped by the Sources read so the untrusted tier
	// stays within the context budget.
	Transcript []Comment
	// Roster is the participants/agents observed in the room, de-duplicated.
	Roster []RosterEntry
}

// RosterEntry is one room participant as the discussion context renders it: the
// display name plus whether it is an agent or a human (so the replying agent
// knows who it can @-mention back).
type RosterEntry struct {
	Name string
	Kind string // "agent" | "human"
}

// Comment is a work-item comment (authoritative tier: part of the §8.5
// work-item "comment history"). Also reused for a discussion transcript line
// (DiscussionContext.Transcript): {author, content, written_at}.
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

// GitHubDetails is the §5.4 SCM-mirror projection for a work item that mirrors
// a GitHub issue — the ones the Epic-2 bridge mints carrying the
// `ksquad.github.issue=owner/repo#N` label (ISI-4757). It exists so a dispatched
// agent sees the upstream issue it is actually working (ISI-5279 WS-D, parent
// ISI-5270): the stable sync FACTS (ref/url/state/last-sync) are control-plane
// observations and ride the best-effort project-meta class, while the issue BODY
// is EXTERNAL content and rides the untrusted-external tier — reference data to
// weigh, never instructions (D8 guardrail). It carries NO secret: the GitHub PAT
// lives in the project credential Secret and never enters the context path.
// The zero value (an ordinary board item, or a mirror not yet caught up) emits
// no element — tolerant exactly like an empty recall/artifact class (AC6).
type GitHubDetails struct {
	IssueRef     string // owner/repo#N (derived from the work-item label)
	IssueURL     string // canonical issue html URL
	State        string // open|closed (mirrored provider state); empty if unmirrored
	Title        string // mirrored issue title; empty if unmirrored
	Body         string // mirrored issue body — EXTERNAL content, untrusted tier
	Actor        string // issue author, provenance for the untrusted body
	LastSyncedAt string // RFC3339 mirror observation (scm.mirror_record.mirrored_at)
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
	// scope, untrusted tier. ids pins the exact doc set (snapshot reuse) and
	// takes precedence; when ids is empty the fresh arm runs a relevance query
	// over queryText (the work item the envelope is about — title/body/AC), so
	// the untrusted-recall tier is populated on first drive, not just on resume.
	// queryText is ignored by the pinned arm (ISI-3607).
	MemoryRecall(ctx context.Context, teamID string, projectID string, queryText string, ids []string, topK int) ([]RecallDoc, error)
	// Artifacts lists the Run's linked artifacts (§5.4 mirror / §6.1
	// artifact rows).
	Artifacts(ctx context.Context, runID string) ([]ArtifactLink, error)
	// GitHubDetails reads the §5.4 SCM-mirror projection of the GitHub issue a
	// work item mirrors (the `ksquad.github.issue=owner/repo#N` label). The zero
	// value (no label, or no mirror row yet) emits nothing — tolerant, like an
	// empty artifact/recall class (AC6). It is deliberately NOT pinned by a
	// revision: the mirror is level-triggered untrusted-external reference, not
	// part of the deterministic-resume snapshot, so it always reads current.
	GitHubDetails(ctx context.Context, projectRef, workItemID string) (GitHubDetails, error)
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
	// RolePrompt is the dispatched agent's resolved Role behavior prompt
	// (Role.Spec.PromptRef, resolved control-plane-side by pkg/roleprompt —
	// ISI-5223). Empty for a role with no prompt. When set it is injected as the
	// FIRST authoritative, must-include element ("roleDirective"): a coordinator
	// role's orchestration instructions (decompose → work_item_create → assign)
	// reach the agent so it delegates rather than working the item as an IC. It
	// is control-plane-authored (never sourced from the sandbox), so it rides the
	// authoritative tier alongside the task directives.
	RolePrompt string
	// TeamRoster is the "your team" fact: the assignable agent NAME ↔ role
	// mapping for this Run's Team (Team.Spec.Agents), resolved control-plane-side
	// by pkg/teamroster (ISI-5245). Empty for a team that names no agents. When
	// set it is injected as an authoritative, must-include element ("teamRoster")
	// right after the roleDirective: a coordinator role tells the PM to
	// work_item_assign each sub-ticket to "an agent on this item's team", and this
	// is the only place the run context names those agents — without it every
	// assign fails as "Invalid agent names". Like the role prompt it is
	// control-plane-authored (never sourced from the sandbox), so it rides the
	// authoritative tier alongside the task directives.
	TeamRoster string
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
	ctx, span := telemetry.Tracer().Start(ctx, "contextasm.assemble", trace.WithAttributes(
		attribute.String("code.namespace", "github.com/K8squad/K8squad/pkg/contextasm"),
		attribute.String("code.function", "Assemble"),
	))
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
	// M1.2 (ISI-4128): scoped recall keys on the project's Postgres uuid
	// (coord.work_item.project_id = Project CR uid) — the ref NAME is not a
	// uuid and fails the scoped-recall query. The resolved CR is in hand.
	recall, err := a.gatherMemoryRecall(ctx, req.TeamID, string(req.Project.UID), recallQueryText(wi), pinnedDocIDs)
	if err != nil {
		return nil, fmt.Errorf("contextasm: memory recall: %w", err)
	}
	arts, err := a.gatherArtifacts(ctx, string(req.Run.UID))
	if err != nil {
		return nil, fmt.Errorf("contextasm: artifacts: %w", err)
	}
	// GitHub issue mirror (ISI-5279 WS-D): current (never pinned) — the mirror is
	// untrusted-external reference, not part of the deterministic-resume snapshot.
	gh, err := a.gatherGitHubDetails(ctx, req.Run.Spec.ProjectRef.Name, req.Run.Spec.WorkItemRef)
	if err != nil {
		return nil, fmt.Errorf("contextasm: github details: %w", err)
	}

	env := a.buildEnvelope(req.RolePrompt, req.TeamRoster, wi, meta, recall, arts, gh, req.Run.Spec.Inputs)

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

func (a *Assembler) gatherMemoryRecall(ctx context.Context, teamID, projectID, queryText string, ids []string) ([]RecallDoc, error) {
	ctx, span := startSourceSpan(ctx, "memory_recall", len(ids) > 0)
	// queryText is NEVER emitted on the span: it is derived from work-item
	// title/body (the highest-PII surface, §8). Only the pinned flag + result
	// count reach telemetry.
	recall, err := a.sources.MemoryRecall(ctx, teamID, projectID, queryText, ids, a.TopK)
	endSourceSpan(span, len(recall), err)
	return recall, err
}

// recallQueryText synthesizes the FRESH memory-recall ANN query from the work
// item the envelope is about (title + body + acceptance criteria). It is
// deterministic for a given work item, so a re-entrant resume that re-reads the
// same pinned work-item revision would derive the identical query — though the
// pinned arm ignores it entirely (ids non-empty), this keeps the fresh arm
// re-entrant by construction (§6.4). It carries only DATA (task content), never
// commands — the recall it seeds is untrusted-tier reference (F16, §7.3).
//
// DISCUSSION RUNS (ISI-5275): a work item minted from an @-mention carries a
// STATIC "go read thread X, reply in the room" instruction as its Title/Body —
// deriving the ANN query from that boilerplate POISONS recall (every discussion
// run queries the same instruction text, never the conversation). So when the
// work item is a discussion run, the query keys off the REAL topic instead: the
// triggering message (weighted first) plus the recent transcript. The work-item
// text is deliberately NOT mixed in — it is the same poison for every room.
func recallQueryText(wi WorkItemFacts) string {
	if d := wi.Discussion; d != nil {
		parts := make([]string, 0, 1+len(d.Transcript))
		seen := make(map[string]bool, 1+len(d.Transcript))
		add := func(s string) {
			if s == "" || seen[s] {
				return
			}
			seen[s] = true
			parts = append(parts, s)
		}
		add(d.TriggerMessage)
		for _, m := range d.Transcript {
			add(m.Content)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		// Empty transcript (a brand-new thread with only the trigger, already
		// de-duped away): fall through to the work-item text rather than query
		// on nothing.
	}
	parts := make([]string, 0, 2+len(wi.AcceptanceCriteria))
	if wi.Title != "" {
		parts = append(parts, wi.Title)
	}
	if wi.Description != "" {
		parts = append(parts, wi.Description)
	}
	parts = append(parts, wi.AcceptanceCriteria...)
	return strings.Join(parts, "\n")
}

func (a *Assembler) gatherArtifacts(ctx context.Context, runID string) ([]ArtifactLink, error) {
	ctx, span := startSourceSpan(ctx, "artifacts", false)
	arts, err := a.sources.Artifacts(ctx, runID)
	endSourceSpan(span, len(arts), err)
	return arts, err
}

// gatherGitHubDetails wraps the §5.4 SCM-mirror read for the work item's GitHub
// issue in a `contextasm.source.github_details` span (o11y §2.2). Never pinned:
// the mirror is level-triggered untrusted-external reference, read current every
// drive (result_count 1 when an issue ref resolved, else 0). It reads only the
// LOCAL mirror — no GitHub call, no credential — so it is a plain DB read latency
// point like the others.
func (a *Assembler) gatherGitHubDetails(ctx context.Context, projectRef, workItemID string) (GitHubDetails, error) {
	ctx, span := startSourceSpan(ctx, "github_details", false)
	gh, err := a.sources.GitHubDetails(ctx, projectRef, workItemID)
	n := 0
	if gh.IssueRef != "" {
		n = 1
	}
	endSourceSpan(span, n, err)
	return gh, err
}

func startSourceSpan(ctx context.Context, source string, pinned bool) (context.Context, trace.Span) {
	return telemetry.Tracer().Start(ctx, "contextasm.source."+source, trace.WithAttributes(
		attribute.String("code.namespace", "github.com/K8squad/K8squad/pkg/contextasm"),
		attribute.String("code.function", "startSourceSpan"),
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
func (a *Assembler) buildEnvelope(rolePrompt, teamRoster string, wi WorkItemFacts, meta ProjectMeta, recall []RecallDoc, arts []ArtifactLink, gh GitHubDetails, inputs map[string]string) *Envelope {
	b := newEnvelopeBuilder()

	// — Role behavior prompt (ISI-5223): the dispatched agent's resolved Role
	// instructions, control-plane-authored, placed FIRST so it frames how the
	// agent works the task (a coordinator role decomposes + delegates rather
	// than acting as an IC). Must-include, never truncated (budget.go) —
	// dropping the persona would silently regress orchestration to IC behavior.
	if rolePrompt != "" {
		b.addAuthoritative("roleDirective", rolePrompt, Provenance{Source: "role"})
	}

	// — Team roster (ISI-5245): the assignable agent NAME ↔ role mapping for the
	// Run's Team, control-plane-authored, placed right after the role directive so
	// a coordinator that has just been told to delegate immediately learns WHO it
	// can delegate to. Must-include, never truncated (budget.go) — dropping it
	// sends the PM back to guessing names, and work_item_assign rejects every
	// invalid name, stalling the assign half of decompose-and-delegate.
	if teamRoster != "" {
		b.addAuthoritative("teamRoster", teamRoster, Provenance{Source: "team"})
	}

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

	// — Discussion-room context (ISI-5275): roster is server-derived participant
	//   names (best-effort project-meta class); the transcript is cross-participant
	//   conversation content and rides the UNTRUSTED-external tier so a room
	//   participant cannot smuggle instructions into the authoritative framing. —
	if d := wi.Discussion; d != nil {
		if roster := formatRoster(d.Roster); roster != "" {
			b.addProjectMeta("roster", roster, Provenance{Source: "discussion"})
		}
		for _, m := range d.Transcript {
			b.addUntrustedExternal("discussionMessage", formatTranscriptLine(m), Provenance{
				Source:    "discussion",
				Author:    m.Author,
				WrittenAt: m.WrittenAt,
			})
		}
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

	// — GitHub issue mirror (ISI-5279 WS-D): a work item that mirrors a GitHub
	//   issue surfaces its sync FACTS (ref/url/state/last-sync — control-plane
	//   observations, best-effort project-meta class) plus the issue BODY. The
	//   body is EXTERNAL content and rides the untrusted-external tier (reference
	//   to WEIGH, never commands); the facts are our own mirror metadata, not
	//   external text, so they sit with the other project-meta facts. The PAT
	//   never reaches here — only already-mirrored local data (D8). —
	if gh.IssueRef != "" {
		fact := gh.IssueRef
		if gh.State != "" {
			fact += " [" + gh.State + "]"
		}
		if gh.IssueURL != "" {
			fact += " " + gh.IssueURL
		}
		if gh.LastSyncedAt != "" {
			fact += " (synced " + gh.LastSyncedAt + ")"
		}
		b.addProjectMeta("githubIssue", fact, Provenance{Source: "github"})
		if body := strings.TrimSpace(gh.Body); body != "" {
			header := gh.IssueRef
			if gh.Title != "" {
				header += " — " + gh.Title
			}
			b.addUntrustedExternal("githubIssueBody", header+"\n"+body, Provenance{
				Source:    "github",
				Author:    gh.Actor,
				WrittenAt: gh.LastSyncedAt,
			})
		}
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

// formatRoster renders the room roster as one compact, auditable line per the
// envelope String() convention: "name (kind)", comma-joined. Empty ⇒ "" so the
// caller skips the element entirely rather than inject a blank roster.
func formatRoster(roster []RosterEntry) string {
	parts := make([]string, 0, len(roster))
	for _, r := range roster {
		if r.Name == "" {
			continue
		}
		if r.Kind != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", r.Name, r.Kind))
		} else {
			parts = append(parts, r.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// formatTranscriptLine renders one transcript message as "author: body" (author
// omitted when blank). The content is UNTRUSTED (buildEnvelope tiers it so); the
// author prefix is attribution the injection framing renders as data, matching
// the recall/artifact tiers.
func formatTranscriptLine(m Comment) string {
	if m.Author == "" {
		return m.Content
	}
	return m.Author + ": " + m.Content
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
