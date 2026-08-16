// Package discussion implements per-project discussion rooms backed by Postgres.
// ISI-2147: KSquad per-Project discussion room (memory-backed).
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ============================================================================
// Domain Types
// ============================================================================

// AuthorType identifies whether a message author is an agent, human, or system.
type AuthorType string

const (
	AuthorTypeAgent  AuthorType = "agent"
	AuthorTypeHuman  AuthorType = "human"
	AuthorTypeSystem AuthorType = "system"
)

// MessageKind classifies the intent of a message.
type MessageKind string

const (
	KindMessage     MessageKind = "message"
	KindAnnouncement MessageKind = "announcement"
	KindDecision    MessageKind = "decision"
	KindQuestion    MessageKind = "question"
)

// Room is a persistent discussion space scoped to a project.
type Room struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"projectId"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

// Message is a single threaded entry in a discussion room.
type Message struct {
	ID         uuid.UUID  `json:"id"`
	RoomID     uuid.UUID  `json:"roomId"`
	ParentID   *uuid.UUID `json:"parentId,omitempty"`
	AuthorID   uuid.UUID  `json:"authorId"`
	AuthorType AuthorType `json:"authorType"`
	AuthorName string     `json:"authorName"`
	Body       string     `json:"body"`
	Kind       MessageKind `json:"kind"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	EditedAt   *time.Time `json:"editedAt,omitempty"`

	// Derived fields (not stored)
	Replies []Message `json:"replies,omitempty"`
}

// ============================================================================
// Errors
// ============================================================================

var (
	ErrRoomNotFound     = errors.New("discussion: room not found")
	ErrMessageNotFound  = errors.New("discussion: message not found")
	ErrParentMismatch   = errors.New("discussion: parent message belongs to a different room")
	ErrRoomArchived     = errors.New("discussion: room is archived")
)

// ============================================================================
// Store
// ============================================================================

// Store provides Postgres-backed persistence for discussion rooms.
type Store struct {
	db *sql.DB
}

// NewStore creates a new Store wrapping the given database connection.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// EnsureRoom creates the default room for a project if it does not exist,
// returning the room. This is idempotent and safe to call on every project access.
func (s *Store) EnsureRoom(ctx context.Context, projectID uuid.UUID, name string) (*Room, error) {
	if name == "" {
		name = "general"
	}
	const q = `
		INSERT INTO discussion_rooms (project_id, name)
		VALUES ($1, $2)
		ON CONFLICT (project_id, name) DO UPDATE SET updated_at = now()
		RETURNING id, project_id, name, created_at, updated_at, archived_at
	`
	var r Room
	var archivedAt pq.NullTime
	err := s.db.QueryRowContext(ctx, q, projectID, name).Scan(
		&r.ID, &r.ProjectID, &r.Name, &r.CreatedAt, &r.UpdatedAt, &archivedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("ensure room: %w", err)
	}
	if archivedAt.Valid {
		r.ArchivedAt = &archivedAt.Time
	}
	return &r, nil
}

// GetRoom retrieves a room by ID.
func (s *Store) GetRoom(ctx context.Context, roomID uuid.UUID) (*Room, error) {
	const q = `
		SELECT id, project_id, name, created_at, updated_at, archived_at
		FROM discussion_rooms WHERE id = $1
	`
	var r Room
	var archivedAt pq.NullTime
	err := s.db.QueryRowContext(ctx, q, roomID).Scan(
		&r.ID, &r.ProjectID, &r.Name, &r.CreatedAt, &r.UpdatedAt, &archivedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		r.ArchivedAt = &archivedAt.Time
	}
	return &r, nil
}

// GetProjectRoom returns the named room for a project, creating it if missing.
func (s *Store) GetProjectRoom(ctx context.Context, projectID uuid.UUID, name string) (*Room, error) {
	if name == "" {
		name = "general"
	}
	const q = `
		SELECT id, project_id, name, created_at, updated_at, archived_at
		FROM discussion_rooms WHERE project_id = $1 AND name = $2 AND archived_at IS NULL
	`
	var r Room
	var archivedAt pq.NullTime
	err := s.db.QueryRowContext(ctx, q, projectID, name).Scan(
		&r.ID, &r.ProjectID, &r.Name, &r.CreatedAt, &r.UpdatedAt, &archivedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return s.EnsureRoom(ctx, projectID, name)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt.Valid {
		r.ArchivedAt = &archivedAt.Time
	}
	return &r, nil
}

// ListProjectRooms returns all non-archived rooms for a project.
func (s *Store) ListProjectRooms(ctx context.Context, projectID uuid.UUID) ([]Room, error) {
	const q = `
		SELECT id, project_id, name, created_at, updated_at, archived_at
		FROM discussion_rooms
		WHERE project_id = $1 AND archived_at IS NULL
		ORDER BY created_at
	`
	rows, err := s.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		var r Room
		var archivedAt pq.NullTime
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.CreatedAt, &r.UpdatedAt, &archivedAt); err != nil {
			return nil, err
		}
		if archivedAt.Valid {
			r.ArchivedAt = &archivedAt.Time
		}
		rooms = append(rooms, r)
	}
	return rooms, rows.Err()
}

// ArchiveRoom soft-deletes a room.
func (s *Store) ArchiveRoom(ctx context.Context, roomID uuid.UUID) error {
	const q = `UPDATE discussion_rooms SET archived_at = now() WHERE id = $1 AND archived_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, roomID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrRoomNotFound
	}
	return nil
}

// ============================================================================
// Messages
// ============================================================================

// PostMessage creates a new message in a room.
func (s *Store) PostMessage(ctx context.Context, roomID uuid.UUID, msg Message) (*Message, error) {
	// Verify room exists and is not archived
	r, err := s.GetRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if r.ArchivedAt != nil {
		return nil, ErrRoomArchived
	}

	// Validate parent belongs to same room
	if msg.ParentID != nil {
		var parentRoomID uuid.UUID
		err := s.db.QueryRowContext(ctx,
			`SELECT room_id FROM discussion_messages WHERE id = $1`, *msg.ParentID).Scan(&parentRoomID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		if err != nil {
			return nil, err
		}
		if parentRoomID != roomID {
			return nil, ErrParentMismatch
		}
	}

	if msg.Kind == "" {
		msg.Kind = KindMessage
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	metaJSON, _ := json.Marshal(msg.Metadata)

	const q = `
		INSERT INTO discussion_messages (room_id, parent_id, author_id, author_type, author_name, body, kind, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at
	`
	err = s.db.QueryRowContext(ctx, q,
		roomID, msg.ParentID, msg.AuthorID, msg.AuthorType, msg.AuthorName, msg.Body, msg.Kind, metaJSON,
	).Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("post message: %w", err)
	}
	msg.RoomID = roomID
	return &msg, nil
}

// GetMessages returns messages in a room with optional threading.
// When threadDepth > 0, replies are nested under parents up to the given depth.
// When threadDepth == 0, messages are returned flat.
func (s *Store) GetMessages(ctx context.Context, roomID uuid.UUID, limit, offset int, threadDepth int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	const q = `
		SELECT id, room_id, parent_id, author_id, author_type, author_name, body, kind, metadata, created_at, edited_at
		FROM discussion_messages
		WHERE room_id = $1
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3
	`
	rows, err := s.db.QueryContext(ctx, q, roomID, limit+1000, offset) // fetch extra for threading
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []Message
	for rows.Next() {
		var m Message
		var parentID uuid.NullUUID
		var editedAt pq.NullTime
		var metaBytes []byte
		if err := rows.Scan(
			&m.ID, &m.RoomID, &parentID, &m.AuthorID, &m.AuthorType, &m.AuthorName,
			&m.Body, &m.Kind, &metaBytes, &m.CreatedAt, &editedAt,
		); err != nil {
			return nil, err
		}
		if parentID.Valid {
			pid := parentID.UUID
			m.ParentID = &pid
		}
		if editedAt.Valid {
			m.EditedAt = &editedAt.Time
		}
		if len(metaBytes) > 0 {
			json.Unmarshal(metaBytes, &m.Metadata)
		}
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if threadDepth == 0 {
		return all[:min(len(all), limit)], nil
	}
	return buildThreadTree(all, limit), nil
}

// SearchMessages performs full-text search across a room's messages.
// This is the query interface consumed by the memory service.
func (s *Store) SearchMessages(ctx context.Context, projectID uuid.UUID, query string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	const q = `
		SELECT m.id, m.room_id, m.parent_id, m.author_id, m.author_type, m.author_name,
		       m.body, m.kind, m.metadata, m.created_at, m.edited_at,
		       ts_rank(to_tsvector('english', m.body), plainto_tsquery('english', $2)) AS rank
		FROM discussion_messages m
		JOIN discussion_rooms r ON r.id = m.room_id
		WHERE r.project_id = $1
		  AND r.archived_at IS NULL
		  AND to_tsvector('english', m.body) @@ plainto_tsquery('english', $2)
		ORDER BY rank DESC, m.created_at DESC
		LIMIT $3
	`
	rows, err := s.db.QueryContext(ctx, q, projectID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Message
	for rows.Next() {
		var m Message
		var parentID uuid.NullUUID
		var editedAt pq.NullTime
		var metaBytes []byte
		var rank float64
		if err := rows.Scan(
			&m.ID, &m.RoomID, &parentID, &m.AuthorID, &m.AuthorType, &m.AuthorName,
			&m.Body, &m.Kind, &metaBytes, &m.CreatedAt, &editedAt, &rank,
		); err != nil {
			return nil, err
		}
		if parentID.Valid {
			pid := parentID.UUID
			m.ParentID = &pid
		}
		if editedAt.Valid {
			m.EditedAt = &editedAt.Time
		}
		if len(metaBytes) > 0 {
			json.Unmarshal(metaBytes, &m.Metadata)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

// SearchByAuthor returns messages by an author across all rooms in a project.
func (s *Store) SearchByAuthor(ctx context.Context, projectID, authorID uuid.UUID, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `
		SELECT m.id, m.room_id, m.parent_id, m.author_id, m.author_type, m.author_name,
		       m.body, m.kind, m.metadata, m.created_at, m.edited_at
		FROM discussion_messages m
		JOIN discussion_rooms r ON r.id = m.room_id
		WHERE r.project_id = $1 AND r.archived_at IS NULL AND m.author_id = $2
		ORDER BY m.created_at DESC
		LIMIT $3
	`
	rows, err := s.db.QueryContext(ctx, q, projectID, authorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMessages(rows)
}

// EditMessage updates the body of a message.
func (s *Store) EditMessage(ctx context.Context, messageID uuid.UUID, body string) (*Message, error) {
	const q = `
		UPDATE discussion_messages SET body = $2, edited_at = now()
		WHERE id = $1
		RETURNING id, room_id, parent_id, author_id, author_type, author_name, body, kind, metadata, created_at, edited_at
	`
	var m Message
	var parentID uuid.NullUUID
	var editedAt pq.NullTime
	var metaBytes []byte
	err := s.db.QueryRowContext(ctx, q, messageID, body).Scan(
		&m.ID, &m.RoomID, &parentID, &m.AuthorID, &m.AuthorType, &m.AuthorName,
		&m.Body, &m.Kind, &metaBytes, &m.CreatedAt, &editedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMessageNotFound
	}
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		pid := parentID.UUID
		m.ParentID = &pid
	}
	if editedAt.Valid {
		m.EditedAt = &editedAt.Time
	}
	if len(metaBytes) > 0 {
		json.Unmarshal(metaBytes, &m.Metadata)
	}
	return &m, nil
}

// DeleteMessage hard-deletes a message (cascade removes replies).
func (s *Store) DeleteMessage(ctx context.Context, messageID uuid.UUID) error {
	const q = `DELETE FROM discussion_messages WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, messageID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrMessageNotFound
	}
	return nil
}

// ============================================================================
// MemoryServiceQuery — the interface the memory service uses to index room history
// ============================================================================

// MemoryIndexable is a flat representation of a message optimized for ingestion
// by the memory service / vector embedding pipeline.
type MemoryIndexable struct {
	MessageID   uuid.UUID   `json:"messageId"`
	RoomID      uuid.UUID   `json:"roomId"`
	ProjectID   uuid.UUID   `json:"projectId"`
	AuthorID    uuid.UUID   `json:"authorId"`
	AuthorType  AuthorType  `json:"authorType"`
	AuthorName  string      `json:"authorName"`
	Body        string      `json:"body"`
	Kind        MessageKind `json:"kind"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
}

// ForMemoryIndex returns all messages in a project's rooms as flat indexable records.
// This is the bridge between the discussion room and the memory service.
func (s *Store) ForMemoryIndex(ctx context.Context, projectID uuid.UUID, since time.Time, limit int) ([]MemoryIndexable, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	const q = `
		SELECT m.id, m.room_id, r.project_id, m.author_id, m.author_type, m.author_name,
		       m.body, m.kind, m.metadata, m.created_at
		FROM discussion_messages m
		JOIN discussion_rooms r ON r.id = m.room_id
		WHERE r.project_id = $1 AND r.archived_at IS NULL AND m.created_at >= $2
		ORDER BY m.created_at ASC
		LIMIT $3
	`
	rows, err := s.db.QueryContext(ctx, q, projectID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemoryIndexable
	for rows.Next() {
		var mi MemoryIndexable
		var metaBytes []byte
		if err := rows.Scan(
			&mi.MessageID, &mi.RoomID, &mi.ProjectID, &mi.AuthorID, &mi.AuthorType,
			&mi.AuthorName, &mi.Body, &mi.Kind, &metaBytes, &mi.CreatedAt,
		); err != nil {
			return nil, err
		}
		if len(metaBytes) > 0 {
			json.Unmarshal(metaBytes, &mi.Metadata)
		}
		results = append(results, mi)
	}
	return results, rows.Err()
}

// ============================================================================
// Helpers
// ============================================================================

func buildThreadTree(all []Message, limit int) []Message {
	byID := make(map[uuid.UUID]*Message, len(all))
	var rootIDs []uuid.UUID
	for _, m := range all {
		mc := m
		mc.Replies = nil
		byID[m.ID] = &mc
	}
	for _, m := range all {
		if m.ParentID == nil {
			rootIDs = append(rootIDs, m.ID)
		} else if parent, ok := byID[*m.ParentID]; ok {
			parent.Replies = append(parent.Replies, *byID[m.ID])
		}
	}
	// Build roots from pointers AFTER replies have been attached
	var roots []Message
	for _, id := range rootIDs {
		roots = append(roots, *byID[id])
	}
	if len(roots) > limit {
		roots = roots[:limit]
	}
	return roots
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var results []Message
	for rows.Next() {
		var m Message
		var parentID uuid.NullUUID
		var editedAt pq.NullTime
		var metaBytes []byte
		if err := rows.Scan(
			&m.ID, &m.RoomID, &parentID, &m.AuthorID, &m.AuthorType, &m.AuthorName,
			&m.Body, &m.Kind, &metaBytes, &m.CreatedAt, &editedAt,
		); err != nil {
			return nil, err
		}
		if parentID.Valid {
			pid := parentID.UUID
			m.ParentID = &pid
		}
		if editedAt.Valid {
			m.EditedAt = &editedAt.Time
		}
		if len(metaBytes) > 0 {
			json.Unmarshal(metaBytes, &m.Metadata)
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
