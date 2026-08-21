// Package recallsource is the 6.6 consuming edge: it adapts the memory service's
// scoped recall (ScopedRecall / ScopedRecallByIDs — the ONE untrusted read path)
// to the Context Assembler's Sources.MemoryRecall hook (3.6, §8.5). It is a
// bridge in the discussionindex/handoffmirror discipline: it imports memory (the
// read service) and contextasm (the hook's shape); neither imports the other.
//
// Both arms stay tenancy-stamped and untrusted-tier-stamped by the SERVICE, never
// by this adapter: the fresh arm is a scoped ANN query; the pinned arm (a Run's
// §6.4 envelope snapshot doc ids, for re-entrant resume) is a scope-enforced
// exact-id fetch. A pinned foreign-tenant id is un-returnable — the deny 6.5
// holds on the exact-id path too. Nothing here re-derives trust: every RecallDoc
// carries the envelope's fields verbatim, so 3.6 injects as-is (AC5).
package recallsource

import (
	"context"
	"fmt"

	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/contextasm"
)

// QueryBuilder synthesizes the relevance query for the FRESH recall arm — the
// assembler's hook carries no query text (its Sources interface is
// recall-width-shaped), so the reconciler wiring binds this to whatever the
// envelope is about: typically the work item title + body + the Run's inputs/goal.
// It runs once per fresh recall; keep it cheap and deterministic for a given Run
// (same Run ⇒ same query ⇒ same recall set, the re-entrant determinism §6.4 pins).
type QueryBuilder func(ctx context.Context, teamID, projectID string) string

// RecallSource implements the MemoryRecall slice of contextasm.Sources over the
// memory ReadService. The reconciler composes it into its full Sources struct
// (WorkItem/ProjectMeta/Artifacts come from the apiserver/coord store, not here).
type RecallSource struct {
	reads *memory.ReadService
	query QueryBuilder
}

// NewRecallSource wires the adapter. query may be nil — a nil QueryBuilder makes
// the FRESH arm refuse (an explicit error, never a silently-empty recall); the
// pinned arm works without a query.
func NewRecallSource(reads *memory.ReadService, query QueryBuilder) *RecallSource {
	return &RecallSource{reads: reads, query: query}
}

// MemoryRecall serves the assembler's recall hook:
//
//   - ids non-empty (snapshot reuse): the EXACT pinned doc set via the scoped
//     exact-id read — order preserved, missing/retracted ids absent (snapshot
//     decay, never an error), tenancy still enforced.
//   - ids empty (fresh): a scoped ANN recall of width topK over the query the
//     QueryBuilder synthesizes for this Run.
//
// Every returned RecallDoc is the untrusted envelope verbatim: Author is the
// envelope's attributed principal (text, e.g. "agent-a" — coord principals — or
// the discussion/memory author), Score is derived from the pgvector cosine
// DISTANCE as 1/(1+d) (monotonic: closer ⇒ higher; the pinned arm has no ranking
// and scores 1.0), and Scope renders "team[/project]" for snapshot bookkeeping.
func (s *RecallSource) MemoryRecall(ctx context.Context, teamID string, projectID string, ids []string, topK int) ([]contextasm.RecallDoc, error) {
	if teamID == "" {
		return nil, fmt.Errorf("recallsource: teamID is required (the Run's own tenancy, never widened)")
	}
	var projectPtr *string
	if projectID != "" {
		p := projectID
		projectPtr = &p
	}

	if len(ids) > 0 {
		hits, err := s.reads.ScopedRecallByIDs(ctx, teamID, projectPtr, ids)
		if err != nil {
			return nil, fmt.Errorf("recallsource: pinned recall: %w", err)
		}
		return docs(hits, true), nil
	}

	if s.query == nil {
		return nil, fmt.Errorf("recallsource: fresh recall requested but no QueryBuilder is wired (refusing rather than recalling on an empty query)")
	}
	hits, err := s.reads.ScopedRecall(ctx, teamID, projectPtr, s.query(ctx, teamID, projectID), topK)
	if err != nil {
		return nil, fmt.Errorf("recallsource: fresh recall: %w", err)
	}
	return docs(hits, false), nil
}

// docs projects recall hits into the assembler's RecallDoc shape — envelope
// verbatim, score derived. pinned marks the exact-id arm (no ranking: score 1.0).
func docs(hits []memory.RecallHit, pinned bool) []contextasm.RecallDoc {
	out := make([]contextasm.RecallDoc, 0, len(hits))
	for _, h := range hits {
		d := contextasm.RecallDoc{
			ID:        h.RecordID,
			Content:   h.Envelope.Content,
			Author:    h.Envelope.Author.Principal,
			WrittenAt: h.Envelope.WrittenAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
			Scope:     h.Envelope.Scope.TeamID,
			Score:     1.0,
		}
		if h.Envelope.Scope.ProjectID != nil && *h.Envelope.Scope.ProjectID != "" {
			d.Scope += "/" + *h.Envelope.Scope.ProjectID
		}
		if !pinned {
			// cosine distance d ∈ [0,2]: smaller = closer. 1/(1+d) maps to
			// (1/3, 1] monotonically — "higher = keep longer" holds.
			d.Score = 1 / (1 + h.Distance)
		}
		out = append(out, d)
	}
	return out
}
