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

package scmwriteback

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SQLStore is the production Store over the shared coordination Postgres. It
// reads the terminal signal (coord.audit_log 'run_terminal'), the join label
// (coord.work_item.labels), and the agent attribution (coord.claim.assignee_agent
// — stamped at acquire, retained through the terminal release per ISI-4237), and
// writes the idempotency marker as an append-only 'github_writeback' audit row.
// It adds NO new coord export surface (FR-B3 untouched): every access is plain
// SQL against tables the coord schema already owns.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore binds the store to the coordination Postgres.
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("scmwriteback: nil db")
	}
	return &SQLStore{db: db}, nil
}

// pendingQuery selects, per labelled work item in the project, ONLY its
// most-recent terminal run (DISTINCT ON … ORDER BY created_at DESC; succeeded
// terminals ride the partial idx_audit_log_run_terminal from migration 0023,
// failed/cancelled fall back to idx_audit_log_work_item), then drops any that
// already carry a 'github_writeback' marker for that run. The result is
// therefore at most one pending row per work item, and only when the latest run
// has not been written back — so an operator restart re-derives the exact same
// set and older historical terminals are never back-posted.
const pendingQuery = `
WITH latest AS (
    SELECT DISTINCT ON (t.work_item_id)
           t.work_item_id AS wid,
           t.run_id,
           t.to_state,
           t.id           AS audit_id
      FROM coord.audit_log t
      JOIN coord.work_item wi ON wi.id = t.work_item_id
     WHERE wi.project_id = $1::uuid
       AND t.event_type = 'run_terminal'
       AND t.run_id IS NOT NULL
       AND EXISTS (
           SELECT 1 FROM unnest(wi.labels) l
            WHERE l LIKE 'ksquad.github.issue=%'
       )
     ORDER BY t.work_item_id, t.created_at DESC, t.id DESC
)
SELECT latest.wid::text,
       latest.run_id::text,
       latest.to_state,
       COALESCE(c.assignee_agent, ''),
       wi.title,
       (SELECT l FROM unnest(wi.labels) l
         WHERE l LIKE 'ksquad.github.issue=%' LIMIT 1)
  FROM latest
  JOIN coord.work_item wi ON wi.id = latest.wid
  LEFT JOIN coord.claim  c ON c.work_item_id = latest.wid
 WHERE NOT EXISTS (
        SELECT 1 FROM coord.audit_log wb
         WHERE wb.work_item_id = latest.wid
           AND wb.run_id = latest.run_id
           AND wb.event_type = 'github_writeback'
       )`

// pendingInitialQuery is the initial-findings sibling of pendingQuery (ISI-5602,
// ADR-0029 §3). It is structurally symmetric — DISTINCT ON (work_item_id) latest
// run, same `ksquad.github.issue=%` label guard, same NOT EXISTS dedup shape —
// but keyed on the `initial_findings_authored` audit signal the authoring hook
// writes (ISI-5603, event_type co-written with the agent's `/post-comment
// kind=initial_findings` call), deduped against the DISTINCT `github_writeback_initial`
// marker, and it carries the authored note itself (payload->>'body') through so
// the write-back has the text without re-reading coord.comment. The two legs
// dedup independently (distinct marker event types), so each fires exactly once
// per run and the initial leg naturally precedes the terminal one (earlier signal).
//
// ponytail: no partial index covers `initial_findings_authored` the way migration
// 0023's idx_audit_log_run_terminal covers terminals; this falls back to
// idx_audit_log_work_item. The per-project pending set is small (github-labelled
// items only), so the scan is bounded; upgrade path if a hot project ever needs
// it = a partial index on (work_item_id, created_at) WHERE
// event_type='initial_findings_authored', mirroring 0023.
const pendingInitialQuery = `
WITH latest AS (
    SELECT DISTINCT ON (t.work_item_id)
           t.work_item_id        AS wid,
           t.run_id,
           t.payload->>'body'    AS body,
           t.id                  AS audit_id
      FROM coord.audit_log t
      JOIN coord.work_item wi ON wi.id = t.work_item_id
     WHERE wi.project_id = $1::uuid
       AND t.event_type = 'initial_findings_authored'
       AND t.run_id IS NOT NULL
       AND EXISTS (
           SELECT 1 FROM unnest(wi.labels) l
            WHERE l LIKE 'ksquad.github.issue=%'
       )
     ORDER BY t.work_item_id, t.created_at DESC, t.id DESC
)
SELECT latest.wid::text,
       latest.run_id::text,
       COALESCE(latest.body, ''),
       COALESCE(c.assignee_agent, ''),
       wi.title,
       (SELECT l FROM unnest(wi.labels) l
         WHERE l LIKE 'ksquad.github.issue=%' LIMIT 1)
  FROM latest
  JOIN coord.work_item wi ON wi.id = latest.wid
  LEFT JOIN coord.claim  c ON c.work_item_id = latest.wid
 WHERE NOT EXISTS (
        SELECT 1 FROM coord.audit_log wb
         WHERE wb.work_item_id = latest.wid
           AND wb.run_id = latest.run_id
           AND wb.event_type = 'github_writeback_initial'
       )`

// createdItemsQuery lists the sub-tickets a run authored via work_item_create,
// in creation order. It reads the SAME run-scoped signal the per-run authoring
// budget counter uses (pkg/coord AgentCreateWorkItem stamps run_id on every
// `work_item_created` audit row): work_item_id is the created child's id and
// payload->>'title' its title at creation. No new coord export surface (FR-B3
// untouched) — plain SQL over a table coord already owns. The row is written
// only on the agent MCP path (the REST create path leaves run_id null), which is
// exactly the dispatched-agent decomposition this comment reports.
const createdItemsQuery = `
SELECT work_item_id::text,
       COALESCE(payload->>'title', '')
  FROM coord.audit_log
 WHERE run_id = $1::uuid
   AND event_type = 'work_item_created'
 ORDER BY created_at ASC, id ASC`

// PendingWriteBacks runs pendingQuery, strips the label prefix down to the bare
// `owner/repo#N` issue ref the engine parses, and attaches the sub-tickets each
// run authored (ISI-4872) so the completion comment reports the true created set.
func (s *SQLStore) PendingWriteBacks(ctx context.Context, projectID string) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx, pendingQuery, projectID)
	if err != nil {
		return nil, fmt.Errorf("scmwriteback: query pending: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		var label sql.NullString
		if err := rows.Scan(&p.WorkItemID, &p.RunID, &p.TerminalStep, &p.AgentName, &p.Title, &label); err != nil {
			return nil, fmt.Errorf("scmwriteback: scan pending: %w", err)
		}
		if !label.Valid || len(label.String) <= len(githubIssueLabelPrefix) {
			// A row that matched the LIKE but not the exact prefix strip is
			// malformed; skip it rather than post to a bogus ref.
			continue
		}
		p.IssueRef = label.String[len(githubIssueLabelPrefix):]
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scmwriteback: iterate pending: %w", err)
	}

	// Second pass (the cursor above must be drained before a new query on the
	// same connection): attach each run's authored sub-tickets. A per-run read
	// failure is not fatal — the comment degrades to the outcome line without the
	// created list rather than wedging the whole write-back pass.
	for i := range out {
		if out[i].RunID == "" {
			continue
		}
		items, cerr := s.createdItems(ctx, out[i].RunID)
		if cerr != nil {
			return nil, cerr
		}
		out[i].CreatedItems = items
	}
	return out, nil
}

// PendingInitialWriteBacks runs pendingInitialQuery and strips the label prefix
// down to the bare `owner/repo#N` ref, exactly as PendingWriteBacks does, tagging
// each row Kind=initial and carrying the authored note through Body. Unlike the
// terminal leg it does NOT attach created sub-tickets: the initial note is a
// plain agent-authored summary, not a decomposition report (ADR-0029 §3).
func (s *SQLStore) PendingInitialWriteBacks(ctx context.Context, projectID string) ([]Pending, error) {
	rows, err := s.db.QueryContext(ctx, pendingInitialQuery, projectID)
	if err != nil {
		return nil, fmt.Errorf("scmwriteback: query pending initial: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		var body, label sql.NullString
		if err := rows.Scan(&p.WorkItemID, &p.RunID, &body, &p.AgentName, &p.Title, &label); err != nil {
			return nil, fmt.Errorf("scmwriteback: scan pending initial: %w", err)
		}
		if !label.Valid || len(label.String) <= len(githubIssueLabelPrefix) {
			// Matched the LIKE but not the exact prefix strip — malformed; skip
			// rather than post to a bogus ref (same guard as the terminal leg).
			continue
		}
		p.IssueRef = label.String[len(githubIssueLabelPrefix):]
		p.Kind = KindInitial
		p.Body = body.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scmwriteback: iterate pending initial: %w", err)
	}
	return out, nil
}

// createdItems reads the sub-tickets one run authored (createdItemsQuery).
func (s *SQLStore) createdItems(ctx context.Context, runID string) ([]CreatedItem, error) {
	rows, err := s.db.QueryContext(ctx, createdItemsQuery, runID)
	if err != nil {
		return nil, fmt.Errorf("scmwriteback: query created items: %w", err)
	}
	defer rows.Close()

	var out []CreatedItem
	for rows.Next() {
		var it CreatedItem
		if err := rows.Scan(&it.ID, &it.Title); err != nil {
			return nil, fmt.Errorf("scmwriteback: scan created item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scmwriteback: iterate created items: %w", err)
	}
	return out, nil
}

// RecordWriteBack appends the append-only idempotency marker for one write-back
// transition under eventType — `github_writeback` (terminal leg) or
// `github_writeback_initial` (initial-findings leg, ISI-5602). The
// (work_item_id, run_id, eventType) triple is the dedup key the matching pending
// query's NOT EXISTS reads back; the two legs use distinct event types so they
// dedup independently and each fires exactly once per run. An empty eventType
// defaults to the terminal marker for back-compat.
//
// ponytail: no unique index enforces one marker per (work_item, run, event_type)
// — dedup relies on the operator being a single leader-elected writer
// (controller-runtime leader election), so two passes never race the same
// project. Upgrade path if that ever changes: a partial unique index on
// (work_item_id, run_id, event_type) + ON CONFLICT DO NOTHING.
func (s *SQLStore) RecordWriteBack(ctx context.Context, workItemID, runID, eventType, note string) error {
	if eventType == "" {
		eventType = markerTerminal
	}
	payload, err := json.Marshal(map[string]string{"result": note})
	if err != nil {
		return fmt.Errorf("scmwriteback: marshal marker: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, payload)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5::jsonb)`,
		workItemID, runID, eventType, WriteBackPrincipal, string(payload)); err != nil {
		return fmt.Errorf("scmwriteback: insert marker: %w", err)
	}
	return nil
}
