package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// ============================================================================
// Diary ergonomics — diary_append / diary_read (Story 6.2 §8.3, ISI-4077)
// ============================================================================
//
// SUBSTRATE DECISION (ratified, ISI-4077 — supersedes arch §7.2's separate `diary_entry` table).
// A diary entry is a `kind='diary'` row on memory.memory_records (see KindDiary in write.go), NOT the
// dedicated `diary_entry(id, agent_id, team_id, entry, created_at)` table §7.2 sketched. The kind-on-
// memory_records substrate shipped with 6.3 and is kept for three reasons:
//   1. ONE vector space — a diary entry is recallable by memory_search / ScopedRecall like any other
//      knowledge, with no second index or cross-table union at read time.
//   2. ONE trust envelope — diary reads ride the SAME untrusted-provenance envelope (§7.3.2, INV1) as
//      every other read; a separate table would need its own envelope projection to avoid a second
//      trust model.
//   3. ONE tenancy/authorship spine — squad_id + principal_id/agent_id/run_id are already NOT-NULL-
//      disciplined and header-stamped (WINV1/WINV2), so §6.5 write-isolation holds for diary for free.
// The §7.2 separate-table design is therefore SUPERSEDED. What this story adds is the ergonomics the
// three-tool surface did not cover: a fixed-kind append and a NON-semantic chronological read the ANN
// Search path cannot express.
//
// diary_read is the store's ONLY ordered-by-time read: `ORDER BY created_at DESC` over one agent's
// diary rows within the caller team. It is still tenancy-scoped (squad_id bites first, so a foreign
// tenant's rows are unreachable) and retracted-excluded, exactly like every other read. Reads are
// TEAM-scoped, not principal-isolated: all diary rows are already team-wide recallable via
// memory_search, so diary_read reading another agent's diary within the same team is consistent with
// the existing untrusted, team-shared knowledge model. The §6.5 isolation invariant ("one principal
// cannot write another's diary") is a WRITE-side property — authorship is stamped from the caller's
// headers, never an argument — and is preserved unchanged.

// DiaryQuery is a chronological, per-agent diary read. Unlike SearchQuery it carries NO embedding:
// diary_read is an ORDER BY created_at DESC scan, not an ANN ranking. SquadID (caller tenant) and
// AgentID (whose diary) are both required; the kind is fixed to KindDiary in the query below, never a
// caller argument.
type DiaryQuery struct {
	SquadID string
	AgentID string
	Limit   int
}

// DiaryRead is the chronological per-agent diary read backing the `diary_read(agent, last_n)` tool
// (§8.3 / ISI-4077). It is the store's ONLY non-semantic ordered read: the caller team's diary rows for
// one agent, newest first, retracted rows excluded, LIMIT n — the "last N diary entries" the ANN Search
// path cannot express. Still tenancy-scoped (squad_id) exactly like every other read: an agent id from
// a foreign tenant returns nothing because the squad predicate bites first. Not part of the Backend
// seam — a diary-read store companion like SearchByIDs.
func (s *PgVectorStore) DiaryRead(ctx context.Context, q DiaryQuery) ([]SearchHit, error) {
	if q.SquadID == "" {
		return nil, fmt.Errorf("diary read: squad_id is required — the diary read is tenancy-scoped (AC1)")
	}
	if q.AgentID == "" {
		return nil, fmt.Errorf("diary read: agent_id is required — diary_read reads exactly ONE agent's diary")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	// agent_id / squad_id are uuid columns; pgx infers the uuid type for the equality params exactly as
	// the semantic Search path binds squad_id = $1 (an invalid-uuid agent arg surfaces as a legible
	// bind error, never a silent empty read). kind is bound as a param, not interpolated.
	const q1 = `
		SELECT id, squad_id, project_id, principal_id, run_id, agent_id, kind, content,
		       created_at, invalidated_at, provenance
		FROM memory.memory_records
		WHERE squad_id = $1 AND agent_id = $2 AND kind = $3 AND invalidated_at IS NULL
		ORDER BY created_at DESC
		LIMIT $4`
	rows, err := s.pool.Query(ctx, q1, q.SquadID, q.AgentID, KindDiary, limit)
	if err != nil {
		return nil, fmt.Errorf("diary read: %w", err)
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var prov []byte
		if err := rows.Scan(
			&h.ID, &h.SquadID, &h.ProjectID, &h.PrincipalID, &h.RunID, &h.AgentID,
			&h.Kind, &h.Content, &h.CreatedAt, &h.InvalidatedAt, &prov,
		); err != nil {
			return nil, fmt.Errorf("scan diary hit: %w", err)
		}
		h.Provenance = json.RawMessage(prov)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate diary hits: %w", err)
	}
	return hits, nil
}

// diaryReader is the optional chronological-by-agent slice of the store the diary_read path needs. The
// Backend seam stays untouched (its fakes keep compiling); PgVectorStore satisfies it at compile time.
type diaryReader interface {
	DiaryRead(ctx context.Context, q DiaryQuery) ([]SearchHit, error)
}

// ensure the concrete store carries the diary read path (compile-time wiring guarantee, not runtime).
var _ diaryReader = (*PgVectorStore)(nil)

// DiaryRead is the `diary_read(agent, last_n)` tool (§8.3 / ISI-4077): the caller team's last N diary
// entries for one agent, newest first, projected through the SAME untrusted envelope as every other
// read (INV1). teamID is the server-authenticated caller tenant (never an argument, INV3); agentID
// selects whose diary within that team (a narrowing filter, not a scope-widener — a foreign tenant's
// rows are unreachable because the squad predicate bites first). lastN defaults to 20 and is capped at
// 100 so "last N" can never ask for the whole substrate.
func (s *ReadService) DiaryRead(ctx context.Context, teamID, agentID string, lastN int) ([]Envelope, error) {
	if teamID == "" {
		return nil, fmt.Errorf("diary_read: caller team scope is required (server-authenticated, never a request arg)")
	}
	if agentID == "" {
		return nil, fmt.Errorf("diary_read: agent is required (whose diary to read within the caller team)")
	}
	dr, ok := s.backend.(diaryReader)
	if !ok {
		return nil, fmt.Errorf("diary_read: the configured backend does not support the chronological diary read")
	}
	if lastN <= 0 {
		lastN = 20
	} else if lastN > 100 {
		lastN = 100
	}
	hits, err := dr.DiaryRead(ctx, DiaryQuery{SquadID: teamID, AgentID: agentID, Limit: lastN})
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(hits))
	for i := range hits {
		out = append(out, buildEnvelope(hits[i]))
	}
	return out, nil
}

// DiaryAppend is the `diary_append(entry)` tool (§8.3 / ISI-4077): ergonomic sugar over the authorized
// write path that commits ONE diary entry. Kind is FIXED to KindDiary (never a caller argument, unlike
// memory_write) and there is no project_id — a diary entry is an agent's per-team work log, not a
// project-scoped note. Author/tenancy are the server-stamped AuthorScope exactly as memory_write, so
// the §6.5 write-isolation invariant (one principal cannot write another's diary) holds by construction:
// authorship is stamped from the caller's headers, never an argument.
func (s *WriteService) DiaryAppend(ctx context.Context, author AuthorScope, entry string) (Record, error) {
	return s.MemoryWrite(ctx, author, KindDiary, entry, nil, nil)
}
