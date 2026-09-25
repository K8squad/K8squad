package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ============================================================================
// The untrusted-provenance read envelope (§7.3.2, Epic 6.4 / Story 10.2 INV1)
// ============================================================================
//
// Every read from the knowledge substrate — a memory record OR a projected discussion message — is
// handed to an agent under ONE shape: cited content, attributed author, and a SERVER-STAMPED trust
// tier that is the constant "untrusted". The room (and memory) is knowledge to *weigh*, never
// authority to *act on* (arch §7.5, ADR-019). There is NO trusted read path and no bespoke discussion
// read shape: `memory_search` and the scoped `discussion_search(project)` tool project through this one
// builder. A body that tries to smuggle a self-elevating `trust:"trusted"` claim is inert text — the
// tier here is a server constant, never read from the row or the query.

// TrustUntrusted is the ONLY value the server ever stamps on a read. It is a constant, not a column.
const TrustUntrusted = "untrusted"

// KindDiscussion marks a memory record that was projected from a discussion.message row (10.2). The
// discussion read tool narrows on it; the envelope builder reads the honest 10.1 provenance triple
// (which is text, not the uuid substrate columns) back out of the record's provenance for these rows.
const KindDiscussion = "discussion"

// Author is the attributed, server-stamped authorship of a read result. agent-vs-human is DERIVED from
// AgentID (never a stored flag), mirroring discussion.Message.AuthorKind and the memory provenance.
type Author struct {
	Principal string  `json:"principal"`
	AgentID   *string `json:"agent_id"` // nil ⇒ human-authored
	IsAgent   bool    `json:"is_agent"` // derived: AgentID != nil
	RunID     *string `json:"run_id"`   // Run linkage (nil for a console/human post)
}

// Scope is the tenancy scope a result belongs to (§7.3.3). It is the record's own stamped scope, never
// a caller-supplied argument — the read plan already filtered to the caller's tenant.
type Scope struct {
	TeamID    string  `json:"team_id"`
	ProjectID *string `json:"project_id"`
}

// Envelope is the untrusted-provenance read shape (§7.3.2). Field order/names are load-bearing: this is
// the SAME envelope for memory and discussion reads (the discussion-memory falsification bench pins
// `{content, author, written_at, scope, trust}` with trust the constant "untrusted").
type Envelope struct {
	Content   string    `json:"content"`
	Author    Author    `json:"author"`
	WrittenAt time.Time `json:"written_at"`
	Scope     Scope     `json:"scope"`
	Trust     string    `json:"trust"` // ALWAYS TrustUntrusted — a server constant, never from the row
}

// discussionProvenance is the honest 10.1 triple carried in a projected discussion record's provenance
// jsonb (the memory uuid columns can't hold the text principal, so the source-of-truth attribution
// rides here — "provenance in = provenance out", never client-supplied). Written by the indexer.
// The v2 wire fields (ISI-4931, plan §4.1): Audience is the R2 party-visibility key the store's search
// predicate reads back out of this jsonb; Kind/Payload identify proposal messages in the index.
type discussionProvenance struct {
	Source          string          `json:"source"`
	MessageID       string          `json:"message_id"`
	ThreadID        string          `json:"thread_id"`
	AuthorPrincipal string          `json:"author_principal"`
	AuthorAgentID   *string         `json:"author_agent_id"`
	AuthorRunID     *string         `json:"author_run_id"`
	Audience        string          `json:"audience,omitempty"` // "party" | "direct:{agentId}" — omitted on legacy rows (pre-v2 projections ⇒ party)
	Kind            string          `json:"kind,omitempty"`     // "text" | "proposal" | …
	Payload         json.RawMessage `json:"payload,omitempty"`  // structured payload, nil unless kind carries one
	WrittenAt       string          `json:"written_at"`         // RFC3339 — the message's original authored time
}

// ProvenanceSourceDiscussion is the provenance.source value the indexer stamps on discussion rows.
const ProvenanceSourceDiscussion = "discussion"

// ProvenanceSourceHandoff is the provenance.source value the §6.6 handoff mirror stamps on
// handoff-mirror rows (Story 6.6). Like the discussion source, it is the marker buildEnvelope
// keys on to surface the honest TEXT provenance (coord principals are text; the memory uuid
// substrate columns carry deterministic derivations) back out of the record — provenance in =
// provenance out, never laundered through the substrate columns.
const ProvenanceSourceHandoff = "handoff"

// handoffProvenance is the honest §6.5/§6.6 triple-plus carried in a mirrored handoff record's
// provenance jsonb: the coord audit row's own columns (work item, run, fence, audit id, the
// content-addressed uri + sha256) and the handoff's text authorship. Written by the
// handoffmirror package's NewHandoffProvenance; read back verbatim here — nothing is derived
// at read time that was not stamped at write time.
type handoffProvenance struct {
	Source          string  `json:"source"`
	URI             string  `json:"uri"`
	SHA256          string  `json:"sha256"`
	WorkItemID      string  `json:"work_item_id"`
	RunID           string  `json:"run_id"`
	AuditID         int64   `json:"audit_id"`
	FenceToken      *int64  `json:"fence_token"`
	AuthorPrincipal string  `json:"author_principal"`
	AuthorAgentID   *string `json:"author_agent_id"`
	WrittenAt       string  `json:"written_at"` // RFC3339 — the audit row's created_at
}

// NewHandoffProvenance builds the provenance jsonb the §6.6 handoff mirror stamps on a mirrored
// handoff record (Story 6.6 / ISI-2896). Taking primitives (not a coord type) keeps this package
// decoupled from pkg/coord exactly as NewDiscussionProvenance keeps it decoupled from discussion:
// the mirror is a consumer of the coord record, never an import of it. agentID may be nil (a human
// operator handoff); fence may be nil (defensive — the writer always has one).
func NewHandoffProvenance(uri, sha256, workItemID, runID string, auditID int64, fence *int64, principal string, agentID *string, writtenAt time.Time) json.RawMessage {
	p := handoffProvenance{
		Source:          ProvenanceSourceHandoff,
		URI:             uri,
		SHA256:          sha256,
		WorkItemID:      workItemID,
		RunID:           runID,
		AuditID:         auditID,
		FenceToken:      fence,
		AuthorPrincipal: principal,
		AuthorAgentID:   agentID,
		WrittenAt:       writtenAt.Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// NewDiscussionProvenance builds the provenance jsonb the 10.2 indexer stamps on a projected discussion
// record. buildEnvelope reads exactly these fields back out — provenance in = provenance out, no
// laundering. Taking primitives (not the discussion type) keeps this package decoupled from discussion.
// audience/kind/payload are the v2 wire fields (ISI-4931): audience feeds the R2 read predicate,
// kind/payload make proposals findable. An empty audience omits the field (legacy rows read as party).
func NewDiscussionProvenance(messageID, threadID, principal string, agentID, runID *string, audience, kind string, payload json.RawMessage, writtenAt time.Time) json.RawMessage {
	p := discussionProvenance{
		Source:          ProvenanceSourceDiscussion,
		MessageID:       messageID,
		ThreadID:        threadID,
		AuthorPrincipal: principal,
		AuthorAgentID:   agentID,
		AuthorRunID:     runID,
		Audience:        audience,
		Kind:            kind,
		Payload:         payload,
		WrittenAt:       writtenAt.Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// buildEnvelope projects one search hit into the untrusted envelope. For a discussion-projected record
// it surfaces the honest 10.1 text provenance from `provenance`; for a native memory record it surfaces
// the record's own columns. `trust` is ALWAYS the server constant — the seductive-wrong design that
// mines the room as trusted authority is un-representable here.
func buildEnvelope(h SearchHit) Envelope {
	env := Envelope{
		Content:   h.Content,
		WrittenAt: h.CreatedAt,
		Scope:     Scope{TeamID: h.SquadID, ProjectID: h.ProjectID},
		Trust:     TrustUntrusted, // server constant — NEVER read from row/query
	}
	if h.Kind == KindDiscussion {
		var p discussionProvenance
		if len(h.Provenance) > 0 && json.Unmarshal(h.Provenance, &p) == nil && p.Source == ProvenanceSourceDiscussion {
			env.Author = Author{
				Principal: p.AuthorPrincipal,
				AgentID:   p.AuthorAgentID,
				IsAgent:   p.AuthorAgentID != nil,
				RunID:     p.AuthorRunID,
			}
			if t, err := time.Parse(time.RFC3339Nano, p.WrittenAt); err == nil {
				env.WrittenAt = t
			}
			return env
		}
	}
	// Mirrored handoff artifact (6.6): attribution is the honest text provenance the mirror
	// stamped — a coord principal is TEXT ("agent:coder"), not a uuid, and surfacing the
	// substrate derivation would launder authorship. Run linkage prefers the provenance run id
	// (the handing-off Run) over any substrate column.
	if h.Kind == KindHandoffMirror {
		var p handoffProvenance
		if len(h.Provenance) > 0 && json.Unmarshal(h.Provenance, &p) == nil && p.Source == ProvenanceSourceHandoff {
			run := p.RunID
			env.Author = Author{
				Principal: p.AuthorPrincipal,
				AgentID:   p.AuthorAgentID,
				IsAgent:   p.AuthorAgentID != nil,
				RunID:     &run,
			}
			if t, err := time.Parse(time.RFC3339Nano, p.WrittenAt); err == nil {
				env.WrittenAt = t
			}
			return env
		}
	}
	// Native memory record: attribution is the record's own uuid provenance columns.
	env.Author = Author{
		Principal: h.PrincipalID,
		AgentID:   h.AgentID,
		IsAgent:   h.AgentID != nil,
		RunID:     h.RunID,
	}
	return env
}

// ============================================================================
// ReadService — the two untrusted read surfaces (both ride ONE index-backed path)
// ============================================================================

// searcher is the slice of Backend the read tools need (kept narrow so tests can fake it). It carries
// both read shapes the ReadService serves: the ANN Search (memory_search/discussion_search) and the
// chronological ReadChronological (diary_read) — the same tenancy/retraction discipline, two orderings.
type searcher interface {
	Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)
	// ReadChronological backs diary_read(agent, last_n): a team+agent+kind scoped, created_at DESC read
	// with no embedding. It is the time-ordered companion to Search (ISI-4077).
	ReadChronological(ctx context.Context, squadID, agentID, kind string, limit int) ([]SearchHit, error)
}

// ReadService serves the untrusted read tools. It embeds the caller's text query and runs a scoped ANN
// search pushed into pgvector (INV2), then projects every hit through the ONE untrusted envelope (INV1).
// The caller's Team scope is server-authenticated and passed in by the transport (§13 BFF) — it is
// NEVER read from a request body, which is what makes cross-tenant widening un-representable (INV3).
type ReadService struct {
	backend searcher
	embed   Embedder
}

// NewReadService wires the read tools to a backend + embedder.
func NewReadService(backend searcher, embed Embedder) *ReadService {
	return &ReadService{backend: backend, embed: embed}
}

// ReaderIdentity is the authenticated READER of the shared index (risk R2, plan §5/§8 — ISI-4931). The
// transports derive it from the server-stamped session headers (X-Principal-Id / X-Agent-Id), never
// from tool arguments — exactly the WINV1/WINV2 discipline the write path uses. It scopes
// direct-audience discussion records: a `direct:{agentId}` projection is excluded from this reader's
// results unless the reader IS the recipient agent or the message's author. A zero value is a
// party-only reader (every human read without an agent linkage behaves this way) — deny-by-default.
type ReaderIdentity struct {
	Principal string // reader's principal ("" ⇒ no author-visibility match)
	AgentID   string // reader's agent id ("" ⇒ no direct-audience record can surface for this reader)
}

// readerScope projects the ReaderIdentity into the store query's narrowing fields. Empty components
// stay nil so the SQL predicate treats them as "no match possible", never as a widening wildcard.
func (rd ReaderIdentity) readerScope() (readerAgentID, readerPrincipal *string) {
	if rd.AgentID != "" {
		a := rd.AgentID
		readerAgentID = &a
	}
	if rd.Principal != "" {
		p := rd.Principal
		readerPrincipal = &p
	}
	return readerAgentID, readerPrincipal
}

// MemorySearch is the `memory_search` tool: Team-scoped semantic recall across ALL knowledge (native
// memory + projected discussion), returned under the untrusted envelope. `callerTeamID` is the caller's
// authenticated tenant; there is no argument to widen past it (INV3). `rd` is the authenticated reader
// the R2 direct-audience predicate scopes (a zero value ⇒ party-visible records only).
func (s *ReadService) MemorySearch(ctx context.Context, callerTeamID string, rd ReaderIdentity, queryText string, topK int) ([]Envelope, error) {
	readerAgentID, readerPrincipal := rd.readerScope()
	return s.read(ctx, SearchQuery{SquadID: callerTeamID, ReaderAgentID: readerAgentID, ReaderPrincipal: readerPrincipal, Limit: topK}, queryText)
}

// DiscussionSearch is the scoped `discussion_search(project)` MCP tool: narrowed to ONE Project's room
// (kind="discussion"), Team-scoped, retracted-excluded, under the untrusted envelope. Same read path as
// MemorySearch — no bespoke room read shape, no second trust model. `rd` is the authenticated reader
// the R2 direct-audience predicate scopes (a zero value ⇒ party-visible records only).
func (s *ReadService) DiscussionSearch(ctx context.Context, callerTeamID, projectID string, rd ReaderIdentity, queryText string, topK int) ([]Envelope, error) {
	if callerTeamID == "" {
		return nil, fmt.Errorf("discussion_search: caller team scope is required (server-authenticated, §7.3.3)")
	}
	if projectID == "" {
		return nil, fmt.Errorf("discussion_search: project id is required (the room key)")
	}
	kind := KindDiscussion
	readerAgentID, readerPrincipal := rd.readerScope()
	return s.read(ctx, SearchQuery{
		SquadID:         callerTeamID,
		ProjectID:       &projectID,
		Kind:            &kind,
		ReaderAgentID:   readerAgentID,
		ReaderPrincipal: readerPrincipal,
		Limit:           topK,
	}, queryText)
}

// DiaryRead is the `diary_read(agent, last_n)` tool (Story 6.2 diary ergonomics, ISI-4077): the caller
// team's chronological view of ONE agent's diary — the newest `lastN` entries, created_at DESC, each
// projected through the SAME untrusted-provenance envelope (§7.3.2 / INV1) as every other read. team is
// server-authenticated (never an arg, INV3); `agentID` is the requested diary owner (the author_agent_id
// diary_append stamped). §6.5 forbids WRITING another principal's diary; a READ stays within the caller
// team and surfaces nothing memory_search does not already expose for kind=diary rows (they share the one
// vector space + trust envelope), so any team member may read a teammate's diary at the untrusted-
// knowledge tier — knowledge to weigh, never authority to act on.
func (s *ReadService) DiaryRead(ctx context.Context, callerTeamID, agentID string, lastN int) ([]Envelope, error) {
	if callerTeamID == "" {
		return nil, fmt.Errorf("diary_read: caller team scope is required (server-authenticated, §7.3.3)")
	}
	if agentID == "" {
		return nil, fmt.Errorf("diary_read: agent is required (the diary owner)")
	}
	hits, err := s.backend.ReadChronological(ctx, callerTeamID, agentID, KindDiary, lastN)
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(hits))
	for _, h := range hits {
		out = append(out, buildEnvelope(h))
	}
	return out, nil
}

// readHits is the SOLE search path: embed the query, run the scoped ANN search on pgvector, return the
// raw ranked hits. The scope/kind predicates in `q` are pushed into the store (INV2/INV3); the
// retraction filter lives in PgVectorStore.Search (INV4). `q.SquadID` is the authenticated caller
// tenant. Every read surface projects these hits through buildEnvelope itself — there is exactly one
// search plan and exactly one untrusted projection, whichever shape the caller needs on top.
func (s *ReadService) readHits(ctx context.Context, q SearchQuery, queryText string) ([]SearchHit, error) {
	if q.SquadID == "" {
		return nil, fmt.Errorf("read: caller team scope is required (server-authenticated, never widened by a request arg)")
	}
	vec, err := s.embed.Embed(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	q.Embedding = vec
	return s.backend.Search(ctx, q)
}

// read projects the sole search path through the ONE untrusted envelope — the agent-facing tools'
// shape (INV1). ScopedRecall (6.6) projects the same hits into RecallHit instead.
func (s *ReadService) read(ctx context.Context, q SearchQuery, queryText string) ([]Envelope, error) {
	hits, err := s.readHits(ctx, q, queryText)
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(hits))
	for _, h := range hits {
		out = append(out, buildEnvelope(h))
	}
	return out, nil
}
