package apiserver

// inboxmarker.go — ISI-5535 (E1 of ISI-5531, ADR-0026 §6): the per-user read-marker store behind the
// "Needs Human Decision" Inbox unread derivation.
//
// This is a HUMAN-facing read-state surface, not a custody operation — so it lives in the apiserver
// (alongside PostgresAuditTrailReader), NOT in pkg/coord (whose FR-B3 pin forbids chat-shaped /
// non-custody exported surface). It reads and upserts coord.decision_read_marker (migration 0032)
// keyed on (user_principal, item_key); UNREAD itself is DERIVED in the handler (an item with no
// marker, or a marker older than the item's last run), never stored here.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// ReadMarkerStore is the seam the inbox handler rides for per-user seen-state. Production wires the
// Postgres store; tests wire a fake. Seen returns the last-seen instant for each of the requested
// keys the user has marked (absent keys are simply not in the map ⇒ unread). MarkSeen upserts
// seen_at = now() for every key.
type ReadMarkerStore interface {
	Seen(ctx context.Context, userPrincipal string, keys []string) (map[string]time.Time, error)
	MarkSeen(ctx context.Context, userPrincipal string, keys []string) error
	// MarkUnread drops the user's read-markers for the given keys so the items re-derive as unread
	// (ISI-5537 E3, the U / "mark unread" half of the read/unread toggle). Idempotent.
	MarkUnread(ctx context.Context, userPrincipal string, keys []string) error
}

// PostgresReadMarkerStore is the production ReadMarkerStore over coord.decision_read_marker. It holds
// no mutable state beyond *sql.DB, so its methods are safe for concurrent use.
type PostgresReadMarkerStore struct {
	db *sql.DB
}

// NewPostgresReadMarkerStore binds the read-marker reads/writes to db.
func NewPostgresReadMarkerStore(db *sql.DB) *PostgresReadMarkerStore {
	return &PostgresReadMarkerStore{db: db}
}

// Seen returns, for the given user, the seen_at of each requested key that has a marker. An empty
// keys slice short-circuits to an empty map (the all-caught-up / nothing-to-check case). The query
// is a single prefix scan on the PK's leading column, narrowed to the requested keys.
func (s *PostgresReadMarkerStore) Seen(ctx context.Context, userPrincipal string, keys []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if userPrincipal == "" || len(keys) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_key, seen_at
		  FROM coord.decision_read_marker
		 WHERE user_principal = $1 AND item_key = ANY($2)`, userPrincipal, pq.Array(keys))
	if err != nil {
		return nil, fmt.Errorf("apiserver.ReadMarker.Seen: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var at time.Time
		if err := rows.Scan(&key, &at); err != nil {
			return nil, fmt.Errorf("apiserver.ReadMarker.Seen: scan: %w", err)
		}
		out[key] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apiserver.ReadMarker.Seen: iterate: %w", err)
	}
	return out, nil
}

// MarkSeen upserts seen_at = now() for every (user, key). Idempotent: re-marking an already-seen
// item advances its timestamp (ON CONFLICT), never a second row. An empty keys slice is a no-op.
func (s *PostgresReadMarkerStore) MarkSeen(ctx context.Context, userPrincipal string, keys []string) error {
	if userPrincipal == "" || len(keys) == 0 {
		return nil
	}
	// unnest the keys array into one multi-row upsert — one round trip regardless of count.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO coord.decision_read_marker (user_principal, item_key, seen_at)
		SELECT $1, k, now() FROM unnest($2::text[]) AS k
		ON CONFLICT (user_principal, item_key) DO UPDATE SET seen_at = now()`, userPrincipal, pq.Array(keys))
	if err != nil {
		return fmt.Errorf("apiserver.ReadMarker.MarkSeen: %w", err)
	}
	return nil
}

// MarkUnread deletes the user's read-markers for the given keys. With no marker, the handler derives
// the item as unread again (ADR-0026 §6 — unread is derived, never stored). Idempotent: deleting a
// key with no marker is a no-op. An empty keys slice is a no-op.
func (s *PostgresReadMarkerStore) MarkUnread(ctx context.Context, userPrincipal string, keys []string) error {
	if userPrincipal == "" || len(keys) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM coord.decision_read_marker
		 WHERE user_principal = $1 AND item_key = ANY($2)`, userPrincipal, pq.Array(keys))
	if err != nil {
		return fmt.Errorf("apiserver.ReadMarker.MarkUnread: %w", err)
	}
	return nil
}
