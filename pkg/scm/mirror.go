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
	"strings"
	"sync"
	"time"
)

// Trust levels stamped on every mirror row (§7.3.2, story 11.1 AC6). The
// mirror is UNTRUSTED-EXTERNAL by construction: the inbound reconciler only
// ever writes TrustUntrustedExternal, and the schema CHECK constraint
// (db/migrations/0008_scm_mirror.sql) rejects anything else — the CHECK pins
// the single allowed value; there is no second trust level in this schema.
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

// MirrorPayload is the normalized record detail persisted in the mirror's
// `payload` JSONB column — everything NormalizedRecord collects beyond the
// indexed columns (body, url, labels, assignees, timestamps, and the
// kind-specific extras), so a mirror row is self-describing for consumers.
type MirrorPayload struct {
	Body       string    `json:"body,omitempty"`
	URL        string    `json:"url,omitempty"`
	Labels     []string  `json:"labels,omitempty"`
	Assignees  []string  `json:"assignees,omitempty"`
	Number     int       `json:"number,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Merged     bool      `json:"merged,omitempty"`
	HeadRef    string    `json:"head_ref,omitempty"`
	BaseRef    string    `json:"base_ref,omitempty"`
	Conclusion string    `json:"conclusion,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Size       int64     `json:"size,omitempty"`
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
	Payload          json.RawMessage
}

// MirrorStore is the persistence seam for the scm mirror (story 11.1 AC2).
// ApplySnapshot performs ONE level-triggered, idempotent, convergent pass:
// every row is written keyed by (project, kind, external_id) — re-applying
// the same snapshot (a redelivered webhook, a poll tick with no changes)
// never creates a duplicate row — and records that have disappeared from
// the snapshot are removed, so the mirror converges to the provider's
// current state instead of accumulating stale rows. The return value is
// the number of rows THIS pass applied (post echo-suppression), which is
// what Project.status.sync.mirrorRecordCount reports. Echo suppression of
// the bot actor happens in BuildMirrorRows, upstream of the store — a
// caller feeding hand-built rows into ApplySnapshot does NOT get
// suppression.
type MirrorStore interface {
	ApplySnapshot(ctx context.Context, projectNamespace, projectName string, rows []MirrorRow) (applied int, err error)
}

// BuildMirrorRows maps a normalized provider snapshot onto provenanced,
// untrusted-external mirror rows for one Project (story 11.1 AC6). Records
// authored by the bot identity are dropped (echo suppression — the only
// place it happens) so our own reflected writes cannot re-enter as fresh
// inbound changes.
func BuildMirrorRows(projectNamespace, projectName string, provider SourceControlProvider, repoURL string, records []NormalizedRecord, botActor string) []MirrorRow {
	if botActor == "" {
		botActor = DefaultBotActor
	}
	rows := make([]MirrorRow, 0, len(records))
	for _, rec := range records {
		if rec.Actor == botActor {
			continue // echo suppression (OQ13): drop our own reflected write
		}
		payload, err := json.Marshal(MirrorPayload{
			Body:       rec.Body,
			URL:        rec.URL,
			Labels:     rec.Labels,
			Assignees:  rec.Assignees,
			Number:     rec.Number,
			CreatedAt:  rec.CreatedAt,
			UpdatedAt:  rec.UpdatedAt,
			Merged:     rec.Merged,
			HeadRef:    rec.HeadRef,
			BaseRef:    rec.BaseRef,
			Conclusion: rec.Conclusion,
			ExpiresAt:  rec.ExpiresAt,
			Size:       rec.Size,
		})
		if err != nil {
			// MirrorPayload contains only JSON-safe types; a marshal
			// failure would be a programming error. Persist the row
			// without payload detail rather than dropping the record.
			payload = nil
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
			Trust:   TrustUntrustedExternal,
			Payload: payload,
		})
	}
	return rows
}

// InMemoryMirrorStore is a MirrorStore backed by a map — the unit-test
// double for the repo-sync reconciler. It honours the same contract as the
// SQL store: idempotent upsert keyed by (project, kind, external id), with
// records absent from a later snapshot removed (per project), and a return
// value counting THIS pass's rows.
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

func projectPrefix(ns, name string) string {
	return fmt.Sprintf("%s/%s|", ns, name)
}

// ApplySnapshot idempotent-upserts rows keyed by external id and removes
// this project's rows that the snapshot no longer contains — same
// convergence semantics as the SQL store. It returns the number of rows
// applied in THIS pass.
func (s *InMemoryMirrorStore) ApplySnapshot(_ context.Context, ns, name string, rows []MirrorRow) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := projectPrefix(ns, name)
	for key := range s.rows {
		if strings.HasPrefix(key, prefix) {
			delete(s.rows, key)
		}
	}
	for _, row := range rows {
		s.rows[mirrorKey(ns, name, row.Kind, row.ExternalID)] = row
	}
	return len(rows), nil
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

// upsertChunkRows bounds how many rows ride one multi-row INSERT: 7
// parameters per row must stay well under Postgres's 65535-parameter
// protocol limit even after future column additions.
const upsertChunkRows = 500

// applyChunkParams is the per-row parameter count of the chunked upsert.
const applyChunkParams = 7

// buildUpsertChunk renders one chunked multi-row upsert statement. Hoisted
// scalars: $1 namespace, $2 name, $3 trust level, $4 pass timestamp
// (mirrored_at and updated_at). Per-row: kind, external_id, state, title,
// actor, external_origin jsonb, payload jsonb.
func buildUpsertChunk(n int) string {
	var sb strings.Builder
	sb.WriteString(`
INSERT INTO scm.mirror_record
    (project_namespace, project_name, kind, external_id, state, title, actor,
     external_origin, trust_level, payload, mirrored_at, updated_at)
SELECT $1, $2, v.kind, v.external_id, v.state, v.title, v.actor,
       v.origin::jsonb, $3, v.payload::jsonb, $4, $4
  FROM (VALUES `)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := 4 + i*applyChunkParams
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7)
	}
	sb.WriteString(`) AS v(kind, external_id, state, title, actor, origin, payload)
ON CONFLICT (project_namespace, project_name, kind, external_id) DO UPDATE SET
    state           = EXCLUDED.state,
    title           = EXCLUDED.title,
    actor           = EXCLUDED.actor,
    external_origin = EXCLUDED.external_origin,
    trust_level     = EXCLUDED.trust_level,
    payload         = EXCLUDED.payload,
    mirrored_at     = EXCLUDED.mirrored_at,
    updated_at      = EXCLUDED.updated_at`)
	return sb.String()
}

// deleteStaleSQL removes this project's rows whose mirrored_at is older
// than the pass timestamp — every row the pass upserted carries the pass
// timestamp, so what remains older is a record the snapshot no longer
// contains (deleted/closed-out upstream). This is what makes the mirror
// convergent instead of write-only.
const deleteStaleSQL = `
DELETE FROM scm.mirror_record
 WHERE project_namespace = $1 AND project_name = $2 AND mirrored_at < $3`

// ApplySnapshot applies the whole snapshot in ONE transaction: chunked
// multi-row upserts (one round trip per 500 rows) followed by a single
// stale-row delete, committed atomically. A mid-pass failure rolls back —
// the mirror is never left half-applied, which is what the idempotence
// contract on MirrorStore promises. It returns the number of rows applied
// in this pass.
func (s *SQLMirrorStore) ApplySnapshot(ctx context.Context, ns, name string, rows []MirrorRow) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("mirror apply begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	passAt := s.now()

	args := make([]interface{}, 0, 4+upsertChunkRows*applyChunkParams)
	for start := 0; start < len(rows); start += upsertChunkRows {
		end := start + upsertChunkRows
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		args = args[:0]
		args = append(args, ns, name, TrustUntrustedExternal, passAt)
		for _, row := range chunk {
			origin, err := json.Marshal(row.ExternalOrigin)
			if err != nil {
				return 0, fmt.Errorf("marshal external_origin for %s:%s: %w", row.Kind, row.ExternalID, err)
			}
			payload := row.Payload
			if len(payload) == 0 {
				payload = json.RawMessage("null")
			}
			args = append(args,
				string(row.Kind), row.ExternalID, row.State, row.Title, row.Actor,
				string(origin), string(payload),
			)
		}

		if _, err := tx.ExecContext(ctx, buildUpsertChunk(len(chunk)), args...); err != nil {
			return 0, fmt.Errorf("upsert mirror rows %s/%s [%d:%d]: %w", ns, name, start, end, err)
		}
	}

	if _, err := tx.ExecContext(ctx, deleteStaleSQL, ns, name, passAt); err != nil {
		return 0, fmt.Errorf("delete stale mirror rows %s/%s: %w", ns, name, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("mirror apply commit: %w", err)
	}
	return len(rows), nil
}
