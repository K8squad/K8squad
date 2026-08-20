package artifactbrowser

import (
	"context"
	"crypto/sha256"
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
	get     string
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
		// consumer of the record sees the same sequence. The (run_id, created_at, id) index
		// (db/migrations) matches this filter + ORDER BY exactly — no sequential scan.
		list: `
			SELECT id::text, work_item_id::text, run_id::text, kind, uri, sha256, created_at
			  FROM coord.artifact
			 WHERE run_id = $1::uuid
			 ORDER BY created_at, id`,
		get: `
			SELECT id::text, work_item_id::text, run_id::text, kind, uri, sha256, created_at
			  FROM coord.artifact
			 WHERE run_id = $1::uuid AND id = $2::uuid`,
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

// GetByRunAndID returns exactly the named row scoped to the run — a single-row read on the
// (run_id, id) pair rather than filtering the run's whole list in Go.
func (s *ProdStore) GetByRunAndID(ctx context.Context, runID, artifactID string) (Artifact, bool, error) {
	var a Artifact
	err := s.db.QueryRowContext(ctx, s.get, runID, artifactID).Scan(
		&a.ID, &a.WorkItemID, &a.RunID, &a.Kind, &a.URI, &a.SHA256, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("artifactbrowser.ProdStore.GetByRunAndID: %w", err)
	}
	return a, true, nil
}

// Content resolves an artifact's canonical bytes from its uri and verifies them against THIS
// row's sha256 before returning. The underlying resolver re-derives an expected digest by
// joining coord.artifact on uri — uncorrelated with the row being served, and uri is not unique
// in the schema — so when two rows share a uri its check can compare against a different row's
// digest. Verifying here against a.SHA256 (the exact row the caller asked for) makes the
// digest-verified guarantee true regardless of the resolver, and fails closed on any mismatch.
func (s *ProdStore) Content(ctx context.Context, a Artifact) ([]byte, error) {
	raw, err := s.content(ctx, a.URI)
	if err != nil {
		return nil, fmt.Errorf("artifactbrowser.ProdStore.Content: resolve %s: %w", a.URI, err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); sum != a.SHA256 {
		return nil, fmt.Errorf(
			"artifactbrowser.ProdStore.Content: digest mismatch for artifact %s at %s (row sha256 %s, got %s)",
			a.ID, a.URI, a.SHA256, sum)
	}
	return raw, nil
}

var _ Store = (*ProdStore)(nil)
