// Package discussion implements the per-Project discussion room — a threaded, append-only,
// provenance-tagged collaboration surface backed by the Postgres `discussion` schema.
//
// Story 10.1 (ISI-2702), Arch §7.5 / ADR-019. "Conversation, not custody." The room is the Project
// (1:1, keyed by project_id — there is NO room table, R1). Every message is append-only and carries a
// SERVER-STAMPED provenance triple (author_principal / author_agent_id / author_run_id) filled from the
// authenticated context, NEVER from the request body (§7.3.1/§6.5, AC3). The schema has NO
// claim/lease/fence/state/holder column: it cannot be a coordination record by construction (§7.5,
// AC4). Custody of a work item moves ONLY in the fenced `coord` claim tables. Retraction is the soft
// `invalidated_at` stamp (§7.4) — the forward path has no hard delete.
//
// This supersedes the naive ISI-2147 shape (discussion_rooms table, client-supplied author_type/
// author_name, hard delete) — see db/migrations/0004_discussion_schema.sql.
package discussion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Provenance context — the SERVER-STAMPED identity of a writer (§7.3.1/§6.5)
// ============================================================================

// AuthorContext is the authenticated identity + tenancy scope of a caller, derived by the §13 BFF
// authz middleware from the session/token/Run — NEVER from the request body. Every write stamps its
// provenance from here, which is what makes impersonation un-representable (AC3).
type AuthorContext struct {
	Principal string    // authenticated identity (always present); becomes author_principal / created_by
	TeamID    uuid.UUID // caller's authorized Team scope (tenancy root, §7.3.3)
	AgentID   *string   // set ⇒ agent-authored; nil ⇒ human (agent-vs-human is DERIVED, not a flag)
	RunID     *string   // set only when posted from within a Run (R2); nil for a console/human post
	IsAdmin   bool      // an admin may retract another principal's message (author-or-admin, AC surface)
}

func (a AuthorContext) agentID() sql.NullString {
	if a.AgentID != nil {
		return sql.NullString{String: *a.AgentID, Valid: true}
	}
	return sql.NullString{}
}

func (a AuthorContext) runID() sql.NullString {
	if a.RunID != nil {
		return sql.NullString{String: *a.RunID, Valid: true}
	}
	return sql.NullString{}
}

// ============================================================================
// Domain types (mirror the §7.5 columns exactly)
// ============================================================================

// Thread is a thread in a Project's discussion room (discussion.thread).
type Thread struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	TeamID    uuid.UUID `json:"teamId"`
	Title     string    `json:"title"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`

	// Populated by GetThread — the thread's live messages, threaded via parent_id.
	Messages []Message `json:"messages,omitempty"`
}

// Message is an append-only, provenance-tagged entry (discussion.message).
type Message struct {
	ID              uuid.UUID  `json:"id"`
	ThreadID        uuid.UUID  `json:"threadId"`
	ParentID        *uuid.UUID `json:"parentId,omitempty"`
	AuthorPrincipal string     `json:"authorPrincipal"`
	AuthorAgentID   *string    `json:"authorAgentId,omitempty"`
	AuthorRunID     *string    `json:"authorRunId,omitempty"`
	Body            string     `json:"body"`
	CreatedAt       time.Time  `json:"createdAt"`
	InvalidatedAt   *time.Time `json:"invalidatedAt,omitempty"`

	// Derived (not stored): threaded replies, built by GetThread.
	Replies []Message `json:"replies,omitempty"`
}

// AuthorKind derives agent/human from the provenance columns (no stored author_kind flag) — the
// console badge (10.3) renders from exactly this. "agent" ⇔ author_agent_id present.
func (m Message) AuthorKind() string {
	if m.AuthorAgentID != nil {
		return "agent"
	}
	return "human"
}

// Retracted reports whether the message has been soft-retracted (§7.4).
func (m Message) Retracted() bool { return m.InvalidatedAt != nil }

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrThreadNotFound is returned when a thread does not exist OR is not visible to the caller's
	// Team scope. It maps to 404-not-403 so a cross-tenant probe cannot distinguish "absent" from
	// "forbidden" (AC5, mirrors the 8.7d BFF gate).
	ErrThreadNotFound   = errors.New("discussion: thread not found")
	ErrMessageNotFound  = errors.New("discussion: message not found")
	ErrEmptyBody        = errors.New("discussion: message body is required")
	ErrEmptyTitle       = errors.New("discussion: thread title is required")
	ErrNotAuthor        = errors.New("discussion: only the author or an admin may retract a message")
	ErrAlreadyRetracted = errors.New("discussion: message already retracted")
)

// ============================================================================
// Store
// ============================================================================

// Store provides Postgres-backed persistence for the discussion schema.
type Store struct {
	db *sql.DB
}

// NewStore wraps a database connection.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const messageCols = `id, thread_id, parent_id, author_principal, author_agent_id, author_run_id, body, created_at, invalidated_at`

// ----------------------------------------------------------------------------
// Threads
// ----------------------------------------------------------------------------

// ListThreads returns the room's threads for (projectID, caller's teamID), most-recent first, paged.
// Fully-retracted threads (every message soft-retracted) are excluded. Tenancy is enforced in the
// WHERE clause — the room never crosses Team boundaries (AC5, FR-J4).
func (s *Store) ListThreads(ctx context.Context, projectID, teamID uuid.UUID, limit, offset int) ([]Thread, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	const q = `
		SELECT id, project_id, team_id, title, created_by, created_at
		FROM discussion.thread t
		WHERE t.project_id = $1 AND t.team_id = $2
		  AND EXISTS (SELECT 1 FROM discussion.message m
		               WHERE m.thread_id = t.id AND m.invalidated_at IS NULL)
		ORDER BY t.created_at DESC
		LIMIT $3 OFFSET $4`
	rows, err := s.db.QueryContext(ctx, q, projectID, teamID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var threads []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.TeamID, &t.Title, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// OpenThread atomically creates a thread and its first message. created_by, team_id, and the message
// provenance are ALL stamped from auth — nothing about the author comes from the caller's payload (AC3).
func (s *Store) OpenThread(ctx context.Context, projectID uuid.UUID, auth AuthorContext, title, body string) (*Thread, error) {
	if title == "" {
		return nil, ErrEmptyTitle
	}
	if body == "" {
		return nil, ErrEmptyBody
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var t Thread
	err = tx.QueryRowContext(ctx, `
		INSERT INTO discussion.thread (project_id, team_id, title, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id, project_id, team_id, title, created_by, created_at`,
		projectID, auth.TeamID, title, auth.Principal,
	).Scan(&t.ID, &t.ProjectID, &t.TeamID, &t.Title, &t.CreatedBy, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("open thread: %w", err)
	}

	first, err := insertMessage(ctx, tx, t.ID, nil, auth, body)
	if err != nil {
		return nil, fmt.Errorf("open thread first message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.Messages = []Message{*first}
	return &t, nil
}

// GetThread returns a thread (scoped to the caller's Team) with its live messages, threaded via
// parent_id. A thread outside the caller's tenancy is ErrThreadNotFound (404-not-403, AC5).
func (s *Store) GetThread(ctx context.Context, projectID, teamID, threadID uuid.UUID) (*Thread, error) {
	var t Thread
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, team_id, title, created_by, created_at
		FROM discussion.thread
		WHERE id = $1 AND project_id = $2 AND team_id = $3`,
		threadID, projectID, teamID,
	).Scan(&t.ID, &t.ProjectID, &t.TeamID, &t.Title, &t.CreatedBy, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrThreadNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageCols+`
		FROM discussion.message
		WHERE thread_id = $1 AND invalidated_at IS NULL
		ORDER BY created_at ASC`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	t.Messages = buildThreadTree(msgs)
	return &t, nil
}

// ----------------------------------------------------------------------------
// Messages
// ----------------------------------------------------------------------------

// PostMessage appends a message (or reply) to a thread. The thread must be within the caller's Team
// scope (else ErrThreadNotFound, AC5). Provenance is stamped from auth (AC3); the parent-same-thread
// invariant is enforced by the DB trigger and re-checked here for a clean error.
func (s *Store) PostMessage(ctx context.Context, projectID, teamID, threadID uuid.UUID, auth AuthorContext, body string, parentID *uuid.UUID) (*Message, error) {
	if body == "" {
		return nil, ErrEmptyBody
	}
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		return nil, err
	}
	if parentID != nil {
		var pThread uuid.UUID
		err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM discussion.message WHERE id = $1`, *parentID).Scan(&pThread)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		if err != nil {
			return nil, err
		}
		if pThread != threadID {
			return nil, fmt.Errorf("discussion: parent %s belongs to a different thread", *parentID)
		}
	}
	return insertMessage(ctx, s.db, threadID, parentID, auth, body)
}

// Retract soft-retracts a message (§7.4 — sets invalidated_at; no hard delete). Author-or-admin only.
// Tenancy-scoped: a message outside the caller's Team is invisible (ErrMessageNotFound / 404).
func (s *Store) Retract(ctx context.Context, projectID, teamID, threadID, messageID uuid.UUID, auth AuthorContext) error {
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		if errors.Is(err, ErrThreadNotFound) {
			return ErrMessageNotFound
		}
		return err
	}
	var authorPrincipal string
	var invalidatedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT author_principal, invalidated_at FROM discussion.message
		WHERE id = $1 AND thread_id = $2`, messageID, threadID).Scan(&authorPrincipal, &invalidatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMessageNotFound
	}
	if err != nil {
		return err
	}
	if !auth.IsAdmin && authorPrincipal != auth.Principal {
		return ErrNotAuthor
	}
	if invalidatedAt.Valid {
		return ErrAlreadyRetracted
	}
	// The append-only trigger permits exactly this one-way NULL→ts update.
	_, err = s.db.ExecContext(ctx, `
		UPDATE discussion.message SET invalidated_at = now()
		WHERE id = $1 AND invalidated_at IS NULL`, messageID)
	return err
}

// assertThreadInScope returns nil iff the thread exists within (projectID, teamID); else
// ErrThreadNotFound. This is the single tenancy predicate every message path passes (AC5).
func (s *Store) assertThreadInScope(ctx context.Context, projectID, teamID, threadID uuid.UUID) error {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM discussion.thread
		WHERE id = $1 AND project_id = $2 AND team_id = $3`, threadID, projectID, teamID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrThreadNotFound
	}
	return err
}

// ----------------------------------------------------------------------------
// Memory-index projection (the 10.2 bridge — carries the provenance triple)
// ----------------------------------------------------------------------------

// MemoryIndexable is a flat, tenancy-scoped projection of a live message for the memory service's
// pgvector index (10.2). It carries the SAME provenance triple as memory writes so 10.2's shared index
// and untrusted-read envelope apply unchanged.
type MemoryIndexable struct {
	MessageID       uuid.UUID `json:"messageId"`
	ThreadID        uuid.UUID `json:"threadId"`
	ProjectID       uuid.UUID `json:"projectId"`
	TeamID          uuid.UUID `json:"teamId"`
	AuthorPrincipal string    `json:"authorPrincipal"`
	AuthorAgentID   *string   `json:"authorAgentId,omitempty"`
	AuthorRunID     *string   `json:"authorRunId,omitempty"`
	Body            string    `json:"body"`
	CreatedAt       time.Time `json:"createdAt"`
}

// ForMemoryIndex returns live messages in a Project's room (tenancy-scoped) created at/after `since`,
// oldest-first, for incremental indexing by the memory service.
func (s *Store) ForMemoryIndex(ctx context.Context, projectID, teamID uuid.UUID, since time.Time, limit int) ([]MemoryIndexable, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	const q = `
		SELECT m.id, m.thread_id, t.project_id, t.team_id, m.author_principal,
		       m.author_agent_id, m.author_run_id, m.body, m.created_at
		FROM discussion.message m
		JOIN discussion.thread t ON t.id = m.thread_id
		WHERE t.project_id = $1 AND t.team_id = $2
		  AND m.invalidated_at IS NULL AND m.created_at >= $3
		ORDER BY m.created_at ASC
		LIMIT $4`
	rows, err := s.db.QueryContext(ctx, q, projectID, teamID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryIndexable
	for rows.Next() {
		var mi MemoryIndexable
		var agentID, runID sql.NullString
		if err := rows.Scan(&mi.MessageID, &mi.ThreadID, &mi.ProjectID, &mi.TeamID,
			&mi.AuthorPrincipal, &agentID, &runID, &mi.Body, &mi.CreatedAt); err != nil {
			return nil, err
		}
		if agentID.Valid {
			mi.AuthorAgentID = &agentID.String
		}
		if runID.Valid {
			mi.AuthorRunID = &runID.String
		}
		out = append(out, mi)
	}
	return out, rows.Err()
}

// ============================================================================
// Internals
// ============================================================================

// execer is satisfied by both *sql.DB and *sql.Tx so insertMessage works inside or outside a tx.
type execer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// insertMessage writes one provenance-stamped message. author_* / created_at are server-provided; the
// caller CANNOT influence them (AC3).
func insertMessage(ctx context.Context, e execer, threadID uuid.UUID, parentID *uuid.UUID, auth AuthorContext, body string) (*Message, error) {
	m := Message{
		ThreadID:        threadID,
		ParentID:        parentID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
	}
	err := e.QueryRowContext(ctx, `
		INSERT INTO discussion.message (thread_id, parent_id, author_principal, author_agent_id, author_run_id, body)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		threadID, parentID, auth.Principal, auth.agentID(), auth.runID(), body,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var parentID uuid.NullUUID
		var agentID, runID sql.NullString
		var invalidatedAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.ThreadID, &parentID, &m.AuthorPrincipal,
			&agentID, &runID, &m.Body, &m.CreatedAt, &invalidatedAt); err != nil {
			return nil, err
		}
		if parentID.Valid {
			pid := parentID.UUID
			m.ParentID = &pid
		}
		if agentID.Valid {
			m.AuthorAgentID = &agentID.String
		}
		if runID.Valid {
			m.AuthorRunID = &runID.String
		}
		if invalidatedAt.Valid {
			t := invalidatedAt.Time
			m.InvalidatedAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// buildThreadTree nests replies under parents via parent_id adjacency (roots = messages with no
// parent), preserving the input order (created_at ASC, so a parent always precedes its children).
// Nesting is arbitrary-depth. An orphaned reply — one whose parent is absent from the input (e.g.
// filtered out because it was retracted) — surfaces as a root so it is never silently dropped.
func buildThreadTree(all []Message) []Message {
	byID := make(map[uuid.UUID]Message, len(all))
	present := make(map[uuid.UUID]bool, len(all))
	children := make(map[uuid.UUID][]uuid.UUID)
	var rootOrder []uuid.UUID
	for _, m := range all {
		present[m.ID] = true
	}
	for _, m := range all {
		byID[m.ID] = m
		if m.ParentID != nil && present[*m.ParentID] {
			children[*m.ParentID] = append(children[*m.ParentID], m.ID)
		} else {
			rootOrder = append(rootOrder, m.ID) // true root, or orphaned reply
		}
	}
	var build func(id uuid.UUID) Message
	build = func(id uuid.UUID) Message {
		m := byID[id]
		m.Replies = nil
		for _, cid := range children[id] {
			m.Replies = append(m.Replies, build(cid))
		}
		return m
	}
	var roots []Message
	for _, id := range rootOrder {
		roots = append(roots, build(id))
	}
	return roots
}
