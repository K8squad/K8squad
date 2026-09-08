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

// RecallSource implements the MemoryRecall slice of contextasm.Sources over the
// memory ReadService. The reconciler composes it into its full Sources struct
// (WorkItem/ProjectMeta/Artifacts come from the apiserver/coord store, not here).
type RecallSource struct {
	reads *memory.ReadService
}

// NewRecallSource wires the adapter. The FRESH recall query text is now carried
// by the assembler's hook (ISI-3607) — derived from the work item the envelope
// is about — so this adapter no longer synthesizes it.
func NewRecallSource(reads *memory.ReadService) *RecallSource {
	return &RecallSource{reads: reads}
}

// MemoryRecall serves the assembler's recall hook:
//
//   - ids non-empty (snapshot reuse): the EXACT pinned doc set via the scoped
//     exact-id read — order preserved, missing/retracted ids absent (snapshot
//     decay, never an error), tenancy still enforced.
//   - ids empty (fresh): a scoped ANN recall of width topK over queryText — the
//     work item the envelope is about, synthesized by the assembler.
//
// Every returned RecallDoc is the untrusted envelope verbatim: Author is the
// envelope's attributed principal (text, e.g. "agent-a" — coord principals — or
// the discussion/memory author), Score is derived from the pgvector cosine
// DISTANCE as 1/(1+d) (monotonic: closer ⇒ higher; the pinned arm has no ranking
// and scores 1.0), and Scope renders "team[/project]" for snapshot bookkeeping.
//
// A fresh recall with an empty queryText refuses (explicit error, never a
// silently-empty recall that would read as "no memory exists").
func (s *RecallSource) MemoryRecall(ctx context.Context, teamID string, projectID string, queryText string, ids []string, topK int) ([]contextasm.RecallDoc, error) {
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

	if queryText == "" {
		return nil, fmt.Errorf("recallsource: fresh recall requested with an empty query text (refusing rather than recalling on an empty query)")
	}
	hits, err := s.reads.ScopedRecall(ctx, teamID, projectPtr, queryText, topK)
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
