/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package issuesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SQLStore is the production Store over scm.issue_link + coord.work_item +
// coord.audit_log on the shared coordination Postgres (ADR-001 — one more
// table on the store we have, not a new datastore). It holds no mutable Go
// state; every method opens its own transaction, so it is safe for
// concurrent use by many goroutines.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore binds the store to the pgx-backed database/sql pool.
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("issuesync.NewSQLStore: nil db")
	}
	return &SQLStore{db: db}, nil
}

// LinkParams establishes one linkage (the story-11.2 write path behind the
// API). Provenance is a FACT about origin, supplied by the caller:
// 'external-sourced' for an item created FROM a provider issue,
// 'ksquad-native' for an item that existed and is being linked.
type LinkParams struct {
	ProjectNamespace string
	ProjectName      string
	WorkItemID       string
	Provider         string
	Repo             string
	ExternalID       string
	ExternalURL      string
	Direction        string
	Provenance       string
}

// ErrLinkExists is returned by EstablishLink when the external issue or the
// work item is already linked — the bijection (UNIQUE constraints in
// 0013_scm_issue_link.sql) refused the second link. The existing counterpart
// is returned for a precise error message.
type ErrLinkExists struct {
	Existing LinkParams
}

func (e *ErrLinkExists) Error() string {
	return fmt.Sprintf("issuesync: %s/%s#%s already linked to work item %s",
		e.Existing.Provider, e.Existing.Repo, e.Existing.ExternalID, e.Existing.WorkItemID)
}

// ErrNoSuchWorkItem is returned when the link references a work item that
// does not exist (the FK would dangle).
var ErrNoSuchWorkItem = errors.New("issuesync: work item not found")

// EstablishLink upserts one issue⇄work-item link. Idempotent on the exact
// same pair (re-linking item↔issue is a no-op update of direction/url);
// linking EITHER side to a DIFFERENT counterpart fails with ErrLinkExists —
// the bijection is structural (0013), not convention.
func (s *SQLStore) EstablishLink(ctx context.Context, p LinkParams) (Link, error) {
	if p.Direction == "" {
		p.Direction = DirectionInbound
	}
	if p.Provenance != ProvenanceKSquadNative && p.Provenance != ProvenanceExternalSourced {
		return Link{}, fmt.Errorf("issuesync.EstablishLink: provenance must be %q or %q (got %q)",
			ProvenanceKSquadNative, ProvenanceExternalSourced, p.Provenance)
	}

	// Reject a divergent re-link BEFORE the upsert so the unique violation
	// carries the existing counterpart (a bare 23505 would not).
	existing, found, err := s.linkByEitherSide(ctx, p)
	if err != nil {
		return Link{}, err
	}
	if found {
		if existing.WorkItemID == p.WorkItemID && existing.Provider == p.Provider && existing.Repo == p.Repo && existing.ExternalID == p.ExternalID {
			// Same pair: idempotent re-establish — refresh direction/url only.
			return existing, s.updateLinkConfig(ctx, existing.ID, p.Direction, p.ExternalURL)
		}
		return existing, &ErrLinkExists{Existing: LinkParams{
			ProjectNamespace: existing.ProjectNamespace,
			ProjectName:      existing.ProjectName,
			WorkItemID:       existing.WorkItemID,
			Provider:         existing.Provider,
			Repo:             existing.Repo,
			ExternalID:       existing.ExternalID,
		}}
	}

	externalURL := nullString(p.ExternalURL)
	var link Link
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// Belt and braces: the FK would catch it, but a distinct error for a
		// missing item is worth one indexed probe.
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM coord.work_item WHERE id = $1::uuid)`, p.WorkItemID).Scan(&exists); err != nil {
			return fmt.Errorf("probe work item: %w", err)
		}
		if !exists {
			return ErrNoSuchWorkItem
		}

		labels := "[]"
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO scm.issue_link
			       (project_namespace, project_name, work_item_id, provider, repo,
			        external_id, external_url, direction, provenance, last_writer,
			        external_labels)
			VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, 'external', $10::jsonb)
			RETURNING id`,
			p.ProjectNamespace, p.ProjectName, p.WorkItemID, p.Provider, p.Repo,
			p.ExternalID, externalURL, p.Direction, p.Provenance, labels).Scan(&link.ID); err != nil {
			return fmt.Errorf("insert link: %w", err)
		}
		link = Link{
			ID:               link.ID,
			ProjectNamespace: p.ProjectNamespace,
			ProjectName:      p.ProjectName,
			WorkItemID:       p.WorkItemID,
			Provider:         p.Provider,
			Repo:             p.Repo,
			ExternalID:       p.ExternalID,
			ExternalURL:      p.ExternalURL,
			Direction:        p.Direction,
			Provenance:       p.Provenance,
			LastWriter:       WriterExternal,
			ExternalLabels:   []string{},
		}
		return nil
	})
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

// DeleteLink removes a linkage (the issue simply stops being linked; the
// external record stays mirrored, the work item stays put).
func (s *SQLStore) DeleteLink(ctx context.Context, projectNamespace, projectName, externalID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM scm.issue_link
		 WHERE project_namespace = $1 AND project_name = $2 AND external_id = $3`,
		projectNamespace, projectName, externalID)
	if err != nil {
		return fmt.Errorf("issuesync.DeleteLink: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("issuesync.DeleteLink: no link for %s/%s#%s", projectNamespace, projectName, externalID)
	}
	return nil
}

// ListLinks returns the project's issue links, ordered deterministically.
func (s *SQLStore) ListLinks(ctx context.Context, projectNamespace, projectName string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_namespace, project_name, work_item_id, provider, repo,
		       external_id, external_url, direction, provenance, last_writer,
		       external_state, external_labels, external_updated_at,
		       ksquad_updated_at, last_synced_at
		  FROM scm.issue_link
		 WHERE project_namespace = $1 AND project_name = $2
		 ORDER BY external_id`, projectNamespace, projectName)
	if err != nil {
		return nil, fmt.Errorf("issuesync.ListLinks: %w", err)
	}
	defer rows.Close()

	var links []Link
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// ReadWorkItem returns the item's current lane and updated_at.
func (s *SQLStore) ReadWorkItem(ctx context.Context, workItemID string) (WorkItemSnapshot, error) {
	var snap WorkItemSnapshot
	err := s.db.QueryRowContext(ctx, `
		SELECT state, updated_at FROM coord.work_item WHERE id = $1::uuid`, workItemID).
		Scan(&snap.State, &snap.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemSnapshot{}, ErrNoSuchWorkItem
	}
	if err != nil {
		return WorkItemSnapshot{}, fmt.Errorf("issuesync.ReadWorkItem: %w", err)
	}
	return snap, nil
}

// ApplyInbound applies one external-wins decision atomically: CAS the lane
// (label-only applies skip the lane write), write the §6.5 audit row, roll
// the link bookkeeping. ErrLaneRace means the CAS guard missed — nothing
// was written and the next pass re-decides.
func (s *SQLStore) ApplyInbound(ctx context.Context, link Link, apply InboundApply) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		ksquadUpdatedAt := apply.Obs.KSquadUpdatedAt
		if apply.ToState != apply.FromState {
			var writtenAt sql.NullTime
			err := tx.QueryRowContext(ctx, `
				UPDATE coord.work_item
				   SET state = $2, updated_at = now()
				 WHERE id = $1::uuid AND state = $3
				RETURNING updated_at`,
				link.WorkItemID, apply.ToState, apply.FromState).Scan(&writtenAt)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLaneRace
			}
			if err != nil {
				return fmt.Errorf("issuesync.ApplyInbound: lane write: %w", err)
			}
			// Roll the KSquad baseline to the POST-write timestamp: our own
			// inbound write must not look like a fresh KSquad change on the
			// next pass (echo discipline, OQ13).
			if writtenAt.Valid {
				ksquadUpdatedAt = writtenAt.Time
			}
		}

		if err := s.insertAudit(ctx, tx, link.WorkItemID, apply.FromState, apply.ToState, apply.AuditPayload); err != nil {
			return err
		}
		return s.updateBookkeeping(ctx, tx, link.ID, apply.Obs.withKSquad(ksquadUpdatedAt))
	})
}

// ApplyOutbound records one ksquad-wins decision AFTER the provider edit
// succeeded: audit row + bookkeeping. The work item is untouched (its lane
// is the winning state by definition).
func (s *SQLStore) ApplyOutbound(ctx context.Context, link Link, apply OutboundApply) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.insertAudit(ctx, tx, link.WorkItemID, apply.FromState, apply.ToState, apply.AuditPayload); err != nil {
			return err
		}
		return s.updateBookkeeping(ctx, tx, link.ID, apply.Obs)
	})
}

// Observe rolls the bookkeeping forward without applying anything.
func (s *SQLStore) Observe(ctx context.Context, link Link, obs Observation) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.updateBookkeeping(ctx, tx, link.ID, obs)
	})
}

// insertAudit writes the §6.5 provenance row (event_type='issue_sync',
// fence_token NULL — the sync loop holds no custody, ADR-037 discipline).
func (s *SQLStore) insertAudit(ctx context.Context, tx *sql.Tx, workItemID, fromState, toState string, payload json.RawMessage) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, event_type, principal, from_state, to_state, payload)
		VALUES ($1::uuid, 'issue_sync', $2, $3, $4, $5::jsonb)`,
		workItemID, SyncPrincipal, nullString(fromState), nullString(toState), string(payload)); err != nil {
		return fmt.Errorf("issuesync: audit: %w", err)
	}
	return nil
}

// updateBookkeeping rolls the link's LWW baseline forward.
func (s *SQLStore) updateBookkeeping(ctx context.Context, tx *sql.Tx, linkID string, obs Observation) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE scm.issue_link SET
		       external_state     = $2,
		       external_labels    = $3::jsonb,
		       external_updated_at = $4,
		       ksquad_updated_at  = $5,
		       last_writer        = $6,
		       direction          = $7,
		       last_synced_at     = now(),
		       updated_at         = now()
		 WHERE id = $1::uuid`,
		linkID, nullString(obs.ExternalState), labelsJSON(obs.ExternalLabels),
		nullTime(obs.ExternalUpdatedAt), nullTime(obs.KSquadUpdatedAt),
		nullString(obs.LastWriter), nullString(obs.Direction)); err != nil {
		return fmt.Errorf("issuesync: bookkeeping: %w", err)
	}
	return nil
}

// withTx runs fn in one transaction, rolling back on error.
func (s *SQLStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("issuesync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("issuesync: commit: %w", err)
	}
	return nil
}

// linkByEitherSide finds a link occupying EITHER the external issue or the
// work item (the bijection's two halves).
func (s *SQLStore) linkByEitherSide(ctx context.Context, p LinkParams) (Link, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, project_namespace, project_name, work_item_id, provider, repo,
		       external_id, external_url, direction, provenance, last_writer,
		       external_state, external_labels, external_updated_at,
		       ksquad_updated_at, last_synced_at
		  FROM scm.issue_link
		 WHERE (provider, repo, external_id) = ($1, $2, $3)
		    OR work_item_id = $4::uuid
		 LIMIT 1`,
		p.Provider, p.Repo, p.ExternalID, p.WorkItemID)
	link, err := scanLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	return link, true, nil
}

// updateLinkConfig refreshes a same-pair re-establish (direction/url).
func (s *SQLStore) updateLinkConfig(ctx context.Context, linkID, direction, externalURL string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE scm.issue_link SET
		       direction = $2, external_url = COALESCE($3, external_url), updated_at = now()
		 WHERE id = $1::uuid`,
		linkID, direction, nullString(externalURL))
	if err != nil {
		return fmt.Errorf("issuesync: link config: %w", err)
	}
	return nil
}

// rowScanner is the common scan surface of QueryRow and Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanLink(row rowScanner) (Link, error) {
	var link Link
	var externalURL, externalState, lastWriter sql.NullString
	var externalLabels []byte
	var externalUpdatedAt, ksquadUpdatedAt, lastSyncedAt sql.NullTime
	err := row.Scan(&link.ID, &link.ProjectNamespace, &link.ProjectName, &link.WorkItemID,
		&link.Provider, &link.Repo, &link.ExternalID, &externalURL, &link.Direction,
		&link.Provenance, &lastWriter, &externalState, &externalLabels,
		&externalUpdatedAt, &ksquadUpdatedAt, &lastSyncedAt)
	if err != nil {
		return Link{}, fmt.Errorf("issuesync.scanLink: %w", err)
	}
	link.ExternalURL = externalURL.String
	link.LastWriter = lastWriter.String
	link.ExternalState = externalState.String
	if len(externalLabels) > 0 {
		_ = json.Unmarshal(externalLabels, &link.ExternalLabels)
	}
	link.ExternalUpdatedAt = externalUpdatedAt.Time
	link.KSquadUpdatedAt = ksquadUpdatedAt.Time
	link.LastSyncedAt = lastSyncedAt.Time
	return link, nil
}

// withKSquad returns the observation with the KSquad baseline replaced by
// the post-apply timestamp (echo discipline: our own write is not a fresh
// KSquad change).
func (o Observation) withKSquad(t time.Time) Observation {
	o.KSquadUpdatedAt = t
	return o
}

func labelsJSON(labels []string) string {
	if labels == nil {
		labels = []string{}
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
