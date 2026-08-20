package artifactbrowser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/K8squad/K8squad/pkg/coord"
)

// ProdStore is the production Store binding: the shipped coord Postgres schema
// (db/migrations/0001_coord_schema.sql). It holds no mutable Go state beyond the *sql.DB and the
// pinned statements, so it is safe for concurrent use by many goroutines.
//
// Content resolution is coord.AuditHandoffContent — the v1 uri binding: a coord+audit://<id> uri
// addresses the audit_log row whose payload holds the canonical (jsonb-normalized) bytes,
// digest-verified against the registering coord.artifact row's sha256, fail-closed on tamper or
// pointer drift. An 8.x object-store binding replaces this resolver without touching the reader.
type ProdStore struct {
	db      *sql.DB
	list    string
	content func(ctx context.Context, uri string) ([]byte, error)
}

// NewProdStore binds the artifact store to the coordination db.
func NewProdStore(db *sql.DB) (*ProdStore, error) {
	if db == nil {
		return nil, errors.New("artifactbrowser.NewProdStore: nil db")
	}
	return &ProdStore{
		db: db,
		// Deterministic order (created_at, id) — the same order ReadHandoff pins, so every
		// consumer of the record sees the same sequence.
		list: `
			SELECT id::text, work_item_id::text, run_id::text, kind, uri, sha256, created_at
			  FROM coord.artifact
			 WHERE run_id = $1::uuid
			 ORDER BY created_at, id`,
		content: coord.AuditHandoffContent(db),
	}, nil
}

// ListByRun returns the run's coord.artifact rows in record order.
func (s *ProdStore) ListByRun(ctx context.Context, runID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, s.list, runID)
	if err != nil {
		return nil, fmt.Errorf("artifactbrowser.ProdStore.ListByRun: %w", err)
	}
	defer rows.Close()
	var arts []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.WorkItemID, &a.RunID, &a.Kind, &a.URI, &a.SHA256, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("artifactbrowser.ProdStore.ListByRun: scan: %w", err)
		}
		arts = append(arts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifactbrowser.ProdStore.ListByRun: iteration: %w", err)
	}
	return arts, nil
}

// Content resolves an artifact's canonical bytes from its uri, digest-verified.
func (s *ProdStore) Content(ctx context.Context, a Artifact) ([]byte, error) {
	raw, err := s.content(ctx, a.URI)
	if err != nil {
		return nil, fmt.Errorf("artifactbrowser.ProdStore.Content: resolve %s: %w", a.URI, err)
	}
	return raw, nil
}

var _ Store = (*ProdStore)(nil)
