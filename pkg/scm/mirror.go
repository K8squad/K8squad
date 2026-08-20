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

// Trust levels stamped on every mirror row (§7.3.2, story 11.1 AC6). The
// mirror is UNTRUSTED-EXTERNAL by construction: the inbound reconciler only
// ever writes TrustUntrustedExternal, and the schema CHECK constraint
// (db/migrations/0008_scm_mirror.sql) rejects anything else from this path.
const (
	// TrustUntrustedExternal is the only trust level the inbound mirror
	// writes. Mirror rows are never trusted control input.
	TrustUntrustedExternal = "untrusted-external"
)

// DefaultBotActor is the default echo-suppression identity: records authored
// by our own reflected writes carry this actor and are dropped on the way
// back in so a reflected write can never re-enter as a fresh inbound change
// (OQ13 field-ownership/echo-suppression model). The outbound-reflection
// story later origin-marks writes with the same configurable identity.
const DefaultBotActor = "ksquad-bot"

// ExternalOrigin is the provenance envelope every mirror row carries
// (§7.3.2, story 11.1 AC6): provider, repo, external id and actor of the
// external record the row mirrors. It is NOT NULL in the scm schema — a
// provenance-less mirror row is a schema violation, not a default.
type ExternalOrigin struct {
	Provider   string `json:"provider"`
	Repo       string `json:"repo"`
	ExternalID string `json:"external_id"`
	Actor      string `json:"actor"`
}

// MirrorRow is one record of the untrusted-external scm mirror, keyed by
// (project namespace/name, kind, external id). Field ownership is split
// (OQ13): every field here is external-owned, written ONLY by the inbound
// reconciler; KSquad-owned linkage/custody lives in the fenced coordination
// record and is never present here — there is deliberately no claim, lease
// or fence field to set.
type MirrorRow struct {
	ProjectNamespace string
	ProjectName      string
	Kind             RecordType
	ExternalID       string
	State            string
	Title            string
	Actor            string
	ExternalOrigin   ExternalOrigin
	Trust            string
}

// MirrorStore is the persistence seam for the scm mirror (story 11.1 AC2).
// ApplySnapshot performs ONE level-triggered, idempotent upsert pass: every
// row is written keyed by (project, kind, external_id) — re-applying the
// same snapshot (a redelivered webhook, a poll tick with no changes) leaves
// the mirror byte-identical, and a second application never creates a
// duplicate row. Records authored by botActor are echo-suppressed before
// the upsert (AC6 loop prevention).
type MirrorStore interface {
	ApplySnapshot(ctx context.Context, projectNamespace, projectName string, rows []MirrorRow, botActor string) (applied int, err error)
}

// BuildMirrorRows maps a normalized provider snapshot onto provenanced,
// untrusted-external mirror rows for one Project (story 11.1 AC6). Records
// authored by the bot identity are dropped (echo suppression) so our own
// reflected writes cannot re-enter as fresh inbound changes.
func BuildMirrorRows(projectNamespace, projectName string, provider SourceControlProvider, repoURL string, records []NormalizedRecord, botActor string) []MirrorRow {
	if botActor == "" {
		botActor = DefaultBotActor
	}
	rows := make([]MirrorRow, 0, len(records))
	for _, rec := range records {
		if rec.Actor == botActor {
			continue // echo suppression (OQ13): drop our own reflected write
		}
		rows = append(rows, MirrorRow{
			ProjectNamespace: projectNamespace,
			ProjectName:      projectName,
			Kind:             rec.Kind,
			ExternalID:       rec.ExternalID,
			State:            rec.State,
			Title:            rec.Title,
			Actor:            rec.Actor,
			ExternalOrigin: ExternalOrigin{
				Provider:   provider.Name(),
				Repo:       repoURL,
				ExternalID: rec.ExternalID,
				Actor:      rec.Actor,
			},
			Trust: TrustUntrustedExternal,
		})
	}
	return rows
}

// InMemoryMirrorStore is a MirrorStore backed by a map — the unit-test
// double for the repo-sync reconciler. It honours the same contract as the
// SQL store: idempotent upsert keyed by (project, kind, external id).
type InMemoryMirrorStore struct {
	mu   sync.Mutex
	rows map[string]MirrorRow
}

// NewInMemoryMirrorStore returns an empty in-memory mirror.
func NewInMemoryMirrorStore() *InMemoryMirrorStore {
	return &InMemoryMirrorStore{rows: map[string]MirrorRow{}}
}

func mirrorKey(ns, name string, kind RecordType, externalID string) string {
	return fmt.Sprintf("%s/%s|%s|%s", ns, name, kind, externalID)
}

// ApplySnapshot idempotent-upserts rows keyed by external id.
func (s *InMemoryMirrorStore) ApplySnapshot(_ context.Context, ns, name string, rows []MirrorRow, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		s.rows[mirrorKey(ns, name, row.Kind, row.ExternalID)] = row
	}
	return len(s.rows), nil
}

// Rows returns a deterministic (sorted) copy of the stored rows.
func (s *InMemoryMirrorStore) Rows() []MirrorRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MirrorRow, 0, len(s.rows))
	for _, row := range s.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ExternalID < out[j].ExternalID
	})
	return out
}

// SQLMirrorStore is the production MirrorStore over the `scm` schema
// (db/migrations/0008_scm_mirror.sql) on the shared coordination Postgres
// (ADR-001 — one Postgres, one more schema, not a new datastore). It rides
// the same database/sql pgx pool the operator opens for coord.
type SQLMirrorStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLMirrorStore binds a mirror store to the pgx-backed database/sql pool.
func NewSQLMirrorStore(db *sql.DB) *SQLMirrorStore {
	return &SQLMirrorStore{db: db, now: time.Now}
}

// mirrorUpsertSQL upserts one row keyed by (project, kind, external_id) —
// the idempotence contract (story 11.1 AC2). External-owned fields are the
// ONLY columns written; the row's provenance is stamped NOT NULL and its
// trust level pinned to untrusted-external by the schema CHECK (AC6).
const mirrorUpsertSQL = `
INSERT INTO scm.mirror_record
    (project_namespace, project_name, kind, external_id, state, title, actor,
     external_origin, trust_level, mirrored_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)
ON CONFLICT (project_namespace, project_name, kind, external_id) DO UPDATE SET
    state          = EXCLUDED.state,
    title          = EXCLUDED.title,
    actor          = EXCLUDED.actor,
    external_origin = EXCLUDED.external_origin,
    trust_level    = EXCLUDED.trust_level,
    mirrored_at    = EXCLUDED.mirrored_at`

// ApplySnapshot idempotent-upserts every row in one pass.
func (s *SQLMirrorStore) ApplySnapshot(ctx context.Context, ns, name string, rows []MirrorRow, _ string) (int, error) {
	for _, row := range rows {
		origin, err := json.Marshal(row.ExternalOrigin)
		if err != nil {
			return 0, fmt.Errorf("marshal external_origin: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, mirrorUpsertSQL,
			ns, name, string(row.Kind), row.ExternalID, row.State, row.Title,
			row.Actor, string(origin), TrustUntrustedExternal, s.now(),
		); err != nil {
			return 0, fmt.Errorf("upsert mirror row %s/%s:%s:%s: %w", ns, name, row.Kind, row.ExternalID, err)
		}
	}
	return len(rows), nil
}
