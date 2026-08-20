package memory

import (
	"context"
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
