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

package scm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sync-history pass kinds (db/migrations/0029 CHECK). 'webhook'/'poll' are the
// reposync mirror triggers (scmmetrics.Trigger*); 'import' is the bridge issue
// import (ISI-5308); 'manual' is an operator/console "Sync now"; 'backfill' is a
// one-off reconciliation. The schema CHECK pins exactly this set — a new kind is
// a forward migration, never a silent widening.
const (
	SyncKindWebhook  = "webhook"
	SyncKindPoll     = "poll"
	SyncKindImport   = "import"
	SyncKindManual   = "manual"
	SyncKindBackfill = "backfill"
)

// Sync-history outcomes (db/migrations/0029 CHECK). The reposync relay records a
// row only on a completed pass today, so SyncOutcomeSuccess dominates; the enum
// leaves room for the import path to record partials/failures without another
// migration.
const (
	SyncOutcomeSuccess = "success"
	SyncOutcomePartial = "partial"
	SyncOutcomeFailed  = "failed"
)

// SyncHistoryRow is one durable GitHub sync/import pass record
// (db/migrations/0029_scm_github_sync_history.sql). It is pass METADATA only —
// no secret ever rides here (ISI-5279 guardrail): the BYO credential is resolved
// and dropped in the operator and has no column. Principal is server-supplied
// (operator SA / bridge principal), never client text — honest provenance.
type SyncHistoryRow struct {
	ID               int64
	ProjectNamespace string
	ProjectName      string
	Provider         string // "github" when empty on write
	Repo             string // repo URL (public identifier; never a token)
	Kind             string // webhook | poll | import | manual | backfill
	IssueRef         string // bare owner/repo#N for an import pass; "" for a whole-repo mirror tick
	RecordCount      int    // rows the pass applied/imported
	Outcome          string // success | partial | failed ("success" when empty on write)
	Principal        string // who ran the pass (operator SA / bridge)
	Detail           json.RawMessage
	SyncedAt         time.Time // when the pass completed (now() at write when zero)
}

// SyncHistoryStore is the write seam for the GitHub sync-history (ISI-5309).
// RecordSync appends ONE row per completed sync/import pass. It is append-only
// by construction — the table rejects UPDATE/DELETE (0029 trigger), so there is
// deliberately no update/delete method here.
type SyncHistoryStore interface {
	RecordSync(ctx context.Context, row SyncHistoryRow) error
}

// SyncHistoryReader is the read side (the "last-sync history" surface). It
// returns one Project's passes newest-first, bounded by limit — a pure read over
// scm.github_sync_history, no GitHub call and no credential. It is a separate
// seam from SyncHistoryStore for the same one-writer/many-readers reason the
// mirror splits MirrorStore from MirrorReader.
type SyncHistoryReader interface {
	ListSyncHistory(ctx context.Context, projectNamespace, projectName string, limit int) ([]SyncHistoryRow, error)
}

// defaultSyncHistoryLimit bounds a ListSyncHistory call that passes limit <= 0
// so the "last-sync history" surface never unboundedly scans the append-only
// table. The surface shows a short recent window; callers wanting more pass an
// explicit larger limit.
const defaultSyncHistoryLimit = 20

func normalizeSyncRow(row *SyncHistoryRow) {
	if row.Provider == "" {
		row.Provider = "github"
	}
	if row.Outcome == "" {
		row.Outcome = SyncOutcomeSuccess
	}
}

// SQLSyncHistoryStore is the production store/reader over scm.github_sync_history
// on the shared coordination Postgres (ADR-001 — one Postgres, one more table,
// not a new datastore). It rides the same database/sql pgx pool the operator
// opens for coord, exactly like SQLMirrorStore.
type SQLSyncHistoryStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLSyncHistoryStore binds a sync-history store to the pgx-backed pool.
func NewSQLSyncHistoryStore(db *sql.DB) *SQLSyncHistoryStore {
	return &SQLSyncHistoryStore{db: db, now: time.Now}
}

const insertSyncHistorySQL = `
INSERT INTO scm.github_sync_history
    (project_namespace, project_name, provider, repo, kind, issue_ref,
     record_count, outcome, principal, detail, synced_at)
VALUES ($1, $2, $3, NULLIF($4,''), $5, NULLIF($6,''), $7, $8, $9, $10::jsonb, $11)`

// RecordSync appends one pass row. synced_at defaults to now() when the caller
// leaves it zero; provider/outcome default to github/success. A nil/empty detail
// is written as SQL NULL rather than the JSON string "null".
func (s *SQLSyncHistoryStore) RecordSync(ctx context.Context, row SyncHistoryRow) error {
	normalizeSyncRow(&row)
	syncedAt := row.SyncedAt
	if syncedAt.IsZero() {
		syncedAt = s.now()
	}
	var detail interface{}
	if len(row.Detail) > 0 && string(row.Detail) != "null" {
		detail = string(row.Detail)
	}
	if _, err := s.db.ExecContext(ctx, insertSyncHistorySQL,
		row.ProjectNamespace, row.ProjectName, row.Provider, row.Repo, row.Kind, row.IssueRef,
		row.RecordCount, row.Outcome, row.Principal, detail, syncedAt,
	); err != nil {
		return fmt.Errorf("scm: record sync history %s/%s: %w", row.ProjectNamespace, row.ProjectName, err)
	}
	return nil
}

const listSyncHistorySQL = `
SELECT id, provider, COALESCE(repo,''), kind, COALESCE(issue_ref,''),
       record_count, outcome, principal, detail, synced_at
  FROM scm.github_sync_history
 WHERE project_namespace = $1 AND project_name = $2
 ORDER BY id DESC
 LIMIT $3`

// ListSyncHistory returns this Project's passes newest-first (by the monotonic
// id), bounded by limit (defaulted when <= 0). Pure read — no GitHub call, no
// credential.
func (s *SQLSyncHistoryStore) ListSyncHistory(ctx context.Context, ns, name string, limit int) ([]SyncHistoryRow, error) {
	if limit <= 0 {
		limit = defaultSyncHistoryLimit
	}
	rows, err := s.db.QueryContext(ctx, listSyncHistorySQL, ns, name, limit)
	if err != nil {
		return nil, fmt.Errorf("scm: list sync history %s/%s: %w", ns, name, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SyncHistoryRow
	for rows.Next() {
		var (
			r      SyncHistoryRow
			detail []byte
		)
		if err := rows.Scan(&r.ID, &r.Provider, &r.Repo, &r.Kind, &r.IssueRef,
			&r.RecordCount, &r.Outcome, &r.Principal, &detail, &r.SyncedAt); err != nil {
			return nil, fmt.Errorf("scm: scan sync history %s/%s: %w", ns, name, err)
		}
		r.ProjectNamespace = ns
		r.ProjectName = name
		if len(detail) > 0 && string(detail) != "null" {
			r.Detail = json.RawMessage(detail)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scm: iterate sync history %s/%s: %w", ns, name, err)
	}
	return out, nil
}

// InMemorySyncHistoryStore is a SyncHistoryStore + SyncHistoryReader backed by a
// slice — the unit-test double. It honours the same contract: append-only (rows
// only accumulate), newest-first reads bounded by limit.
type InMemorySyncHistoryStore struct {
	mu   sync.Mutex
	rows []SyncHistoryRow
	seq  int64
}

// NewInMemorySyncHistoryStore returns an empty in-memory sync-history store.
func NewInMemorySyncHistoryStore() *InMemorySyncHistoryStore {
	return &InMemorySyncHistoryStore{}
}

// RecordSync appends a row in memory, stamping a monotonic id and defaulting
// synced_at/provider/outcome exactly like the SQL store.
func (s *InMemorySyncHistoryStore) RecordSync(_ context.Context, row SyncHistoryRow) error {
	normalizeSyncRow(&row)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	row.ID = s.seq
	if row.SyncedAt.IsZero() {
		row.SyncedAt = time.Now()
	}
	s.rows = append(s.rows, row)
	return nil
}

// ListSyncHistory returns this Project's rows newest-first (by id), bounded by
// limit (defaulted when <= 0) — the same contract as the SQL reader.
func (s *InMemorySyncHistoryStore) ListSyncHistory(_ context.Context, ns, name string, limit int) ([]SyncHistoryRow, error) {
	if limit <= 0 {
		limit = defaultSyncHistoryLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := make([]SyncHistoryRow, 0, len(s.rows))
	for _, r := range s.rows {
		if r.ProjectNamespace == ns && r.ProjectName == name {
			matched = append(matched, r)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}
