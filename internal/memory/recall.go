package memory

import (
	"context"
	"fmt"
)

// Story 6.6 (ISI-2896) — the scoped-recall seam the Context Assembler (3.6) consumes. The
// assembler runs in the Run reconciler, control-plane side: it is NOT an agent, never
// authenticated as one, and receives the caller's Team scope from the Run's own tenancy
// (Run.spec.teamRef) — the same server-side discipline the §13 BFF applies to agent reads.
// What lands here is deliberately the SAME sole read path (read → scoped ANN on pgvector →
// untrusted envelope) the agent tools ride: no second trust model, no assembler-specific
// trusted variant, no widened scope. Recall is reference for the envelope's memory-recall
// tier (ContextBudget.memoryRecall), never commands (F16, §7.3).

// KindHandoffMirror marks a memory record that was mirrored from a structured handoff
// artifact (Story 2.8 / §6.6): the provenance-tagged reflection of a coord audit row whose
// payload is the canonical HandoffDoc. The migration's kind comment (0001_memory.sql) named
// it; this story makes it a first-class written value. The handoffmirror package is the
// writer; recall surfaces it like any other knowledge — untrusted, attributed, scoped.
const KindHandoffMirror = "handoff-mirror"

// RecallHit pairs the untrusted read envelope with the record id and cosine distance the
// assembler needs for envelope accounting: the §6.4 Run.status envelope snapshot pins the
// resolved memory doc-ids (so a resumed Run can assert identical context), and the tier's
// token budget trims by distance rank. The Envelope stays the load-bearing
// {content, author, written_at, scope, trust} shape — nothing here re-wraps or re-trusts it.
type RecallHit struct {
	// RecordID is the memory doc-id for the §6.4 envelope snapshot (audit + re-entrant reuse).
	RecordID string
	// Distance is the pgvector cosine distance to the recall query (rank already applied).
	Distance float64
	// Envelope is the untrusted-recall tier payload: full provenance, trust the constant
	// "untrusted" — a server stamp, never content the record can influence.
	Envelope Envelope
}

// ScopedRecall is the Story 6.6 recall the Context Assembler calls while assembling a Run's
// envelope (3.6, Claiming → Running). teamID is the Run's own Team (server-derived, never a
// request argument); projectID optionally narrows to the Run's Project (nil ⇒ squad-wide).
// Results ride the ONE read path — scoped ANN pushed into pgvector, retracted rows excluded,
// every hit projected through the untrusted envelope — so the assembler receives recall it
// can cite, never recall it must obey.
func (s *ReadService) ScopedRecall(ctx context.Context, teamID string, projectID *string, queryText string, topK int) ([]RecallHit, error) {
	// Clamp the recall width: topK<=0 defaults to 10; a caller-supplied value >100
	// is bounded to 100. Recall feeds the envelope's memory-recall tier — an
	// unbounded width is an unbounded token budget downstream (5.9 truncates, but
	// the recall seam never asks for the whole substrate in the first place).
	if topK <= 0 {
		topK = 10
	} else if topK > 100 {
		topK = 100
	}
	hits, err := s.readHits(ctx, SearchQuery{SquadID: teamID, ProjectID: projectID, Limit: topK}, queryText)
	if err != nil {
		return nil, err
	}
	out := make([]RecallHit, 0, len(hits))
	for i := range hits {
		out = append(out, RecallHit{
			RecordID: hits[i].ID,
			Distance: hits[i].Distance,
			Envelope: buildEnvelope(hits[i]),
		})
	}
	return out, nil
}

// idSearcher is the optional exact-id slice of the store the pinned path needs. The Backend seam
// stays untouched (its fakes keep compiling); PgVectorStore satisfies this at compile time below.
type idSearcher interface {
	SearchByIDs(ctx context.Context, q SearchQuery, ids []string) ([]SearchHit, error)
}

// ensure the concrete store carries the pinned path (compile-time wiring guarantee, not runtime).
var _ idSearcher = (*PgVectorStore)(nil)

// ScopedRecallByIDs serves the §6.4 snapshot-reuse arm: a resumed Run pins the EXACT doc ids its
// envelope snapshot resolved, and this read returns those records — unchanged, untrusted-envelope-
// projected, and STILL tenancy-scoped (a pinned id from a foreign tenant is un-returnable; the ids
// are the Run's own snapshot, but the service trusts nothing). Missing/soft-retracted ids are
// simply absent — snapshot decay the assembler (3.6) handles, never an error here. Distance is 0
// (no ranking on the pinned path — the order is the requested order).
func (s *ReadService) ScopedRecallByIDs(ctx context.Context, teamID string, projectID *string, ids []string) ([]RecallHit, error) {
	if teamID == "" {
		return nil, fmt.Errorf("recall by ids: caller team scope is required (server-authenticated, never widened by a request arg)")
	}
	byID, ok := s.backend.(idSearcher)
	if !ok {
		return nil, fmt.Errorf("recall by ids: the configured backend does not support the pinned-snapshot read")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	hits, err := byID.SearchByIDs(ctx, SearchQuery{SquadID: teamID, ProjectID: projectID}, ids)
	if err != nil {
		return nil, err
	}
	out := make([]RecallHit, 0, len(hits))
	for i := range hits {
		out = append(out, RecallHit{
			RecordID: hits[i].ID,
			Envelope: buildEnvelope(hits[i]),
		})
	}
	return out, nil
}
