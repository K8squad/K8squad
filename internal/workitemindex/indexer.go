// Package workitemindex is the WS-C bridge (ISI-5277, parent ISI-5270): it projects committed
// coord.work_item rows — title, body, and the item's comment thread — into the ksquad-memory pgvector
// index (behind the Backend seam), so a TICKET becomes semantically RECALLABLE as distrusted,
// attributed, Team-scoped knowledge inside the Context Assembler, exactly as discussionindex (10.2)
// makes a discussion room recallable and handoffmirror (6.6) makes a handoff recallable. It imports the
// coord record (via raw SQL over the shared Postgres) and the memory backend (sink); pkg/coord does not
// import memory and memory does not import pkg/coord — this package is the ONLY coupling point.
//
// Posture (the outbox-relay posture, §17.4 — same as discussionindex/handoffmirror): indexing is
// best-effort and post-commit. The sweep runs on the memory service out of band, pulls work items the
// coord record has ALREADY committed, and can only ever MIRROR the server-stamped row — it invents no
// author (created_by is carried verbatim) and trusts no client-supplied field. A board write or Run
// therefore never waits on, and is never failed by, the indexer; a memory outage never rolls back a
// ticket.
//
// MUTABILITY (the one shape discussionindex does NOT have): a discussion message is immutable, so its
// indexer keys idempotency on the message id and an ON CONFLICT DO NOTHING re-projection. A work item
// is MUTABLE — title/body are edited and comments append — so this bridge keys the memory record id on
// the item id PLUS a revision signature (updated_at ⊔ last-comment-time, and the comment count). An
// unchanged re-sweep re-derives the SAME id and is a no-op; a changed item derives a NEW id, projects a
// fresh row, and the supersede companion soft-retracts the item's earlier revisions so recall surfaces
// only the newest title/body/comments — the republish-retire pattern handoffmirror established.
//
// No-custody: this is a plain provenanced memory write and recall of it is a plain untrusted read.
// Nothing here touches coord.claim or mutates coord.work_item; recalling a ticket confers no custody.
package workitemindex

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/memory"
)

// WorkItemIndexable is a flat, tenancy-scoped projection of one committed work item ready to index: the
// item's own columns plus its aggregated comment thread and the derived last-activity time. It carries
// its OWN project/team scope (each item projects into its own tenant scope, so tenancy is preserved by
// construction on the read side — the scoped recall). TeamID == "" marks an item with no team scope yet
// (§6.1 team is inherited and may be unset): unindexable, since a memory record's squad_id is NOT NULL.
type WorkItemIndexable struct {
	WorkItemID    string
	ProjectID     string
	TeamID        string // "" ⇒ no team scope yet — unindexable (see Sweep)
	Title         string
	Body          string
	State         string
	CreatedBy     string // coord principal (TEXT) — the honest author, carried verbatim into provenance
	CreatedAt     time.Time
	CommentCount  int
	CommentBodies string    // the comment thread, oldest-first, "<principal>: <body>" per line ("" ⇒ no comments)
	ActivityAt    time.Time // GREATEST(updated_at, last comment created_at) — the revision/watermark key
}

// recordID is the deterministic memory record id for ONE revision of a work item: the item id plus a
// revision signature (the last-activity timestamp and the comment count). An unchanged re-sweep derives
// the SAME id (idempotent ON CONFLICT no-op); any edit or new comment advances ActivityAt (and/or the
// count) and derives a NEW id, so the fresh revision is a distinct row the supersede path then promotes.
func recordID(w WorkItemIndexable) string {
	sig := fmt.Sprintf("%s@%s#%d", w.WorkItemID, w.ActivityAt.UTC().Format(time.RFC3339Nano), w.CommentCount)
	return deriveUUID("work-item-record", sig)
}

// principalNamespace is the SAME fixed UUIDv5 namespace discussionindex/handoffmirror derive substrate
// uuids with: a text principal maps to the SAME deterministic uuid across all three bridges, so a
// principal's memory rows join on the substrate columns regardless of which bridge wrote them. The
// honest TEXT attribution still rides verbatim in `provenance` (provenance in = provenance out).
var principalNamespace = uuid.MustParse("6b1e5b1e-2c9a-5e7d-9f3a-10b2c3d4e5f6")

// deriveUUID maps a coord text identity to a deterministic uuid for a memory substrate column (same
// derivation as the discussion/handoff bridges).
func deriveUUID(prefix, text string) string {
	return uuid.NewSHA1(principalNamespace, []byte(prefix+":"+text)).String()
}

// WorkItemSource is the coord side of the bridge (satisfied by *SQLSource). AllForMemoryIndex returns
// the live work items whose last activity is at/after a watermark, oldest-activity-first, each carrying
// its own tenancy and comment thread.
type WorkItemSource interface {
	AllForMemoryIndex(ctx context.Context, since time.Time, limit int) ([]WorkItemIndexable, error)
}

// CursorStore is the durable-watermark seam (satisfied by *memory.PgVectorStore). It persists the
// projection's position so a restart resumes from the last committed watermark rather than re-scanning
// from zero or skipping a window. Optional: a nil store keeps the in-process watermark only (unit tests).
type CursorStore interface {
	LoadProjectionCursor(ctx context.Context, name string) (memory.ProjectionCursor, bool, error)
	SaveProjectionCursor(ctx context.Context, name string, at time.Time, id string) error
}

// Superseder is the republish-retire companion (satisfied by *memory.PgVectorStore). After a fresh
// revision is written it soft-retracts the item's earlier revisions so recall surfaces exactly the
// newest. nil ⇒ republish-retire disabled (unit tests / a sink without the companion).
type Superseder interface {
	SupersedeWorkItemRecords(ctx context.Context, squadID, workItemID, keepID string) (int64, error)
}

// cursorName is the projection_cursor key for the work-item→memory indexer (distinct from the
// discussion indexer's "discussion" cursor — the two projections advance independently).
const cursorName = "work_item"

// Indexer projects committed work items into the memory pgvector index. It keeps an in-process
// watermark (max last-activity indexed) plus a seen-set of record ids, so revision ties at the
// watermark boundary are never double-indexed within a process. With a CursorStore the watermark also
// survives a restart. Re-projection is non-destructive: the deterministic revision record id makes an
// unchanged re-write idempotent, and the supersede companion collapses superseded revisions.
type Indexer struct {
	src       WorkItemSource
	sink      memory.Backend
	embed     memory.Embedder
	supersede Superseder // nil ⇒ republish-retire disabled

	cursor       CursorStore // nil ⇒ in-process watermark only
	cursorLoaded bool

	watermark   time.Time
	watermarkID string // record id at the high-water mark — persisted as tie provenance
	seen        map[string]struct{}
	batchSize   int
}

// NewIndexer wires the bridge. batchSize<=0 defaults to 100. superseder may be nil (republish-retire
// disabled); production wiring passes the *memory.PgVectorStore, which implements it.
func NewIndexer(src WorkItemSource, sink memory.Backend, embed memory.Embedder, superseder Superseder, batchSize int) *Indexer {
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Indexer{
		src:       src,
		sink:      sink,
		embed:     embed,
		supersede: superseder,
		seen:      make(map[string]struct{}),
		batchSize: batchSize,
	}
}

// WithCursor attaches a durable watermark, making the projection restart-surviving. The persisted
// cursor is loaded lazily on the first Sweep and advanced after each sweep that moves the watermark. A
// nil store is a no-op (in-process watermark only). Returns the indexer for chaining.
func (ix *Indexer) WithCursor(store CursorStore) *Indexer {
	ix.cursor = store
	return ix
}

// Sweep projects one batch of not-yet-indexed work-item revisions into the memory index and returns how
// many were newly indexed. Best-effort: a revision that fails to embed or write is logged and skipped
// (retried on the next sweep), never aborting the batch — the board is never blocked.
func (ix *Indexer) Sweep(ctx context.Context) (int, error) {
	ix.ensureCursorLoaded(ctx)

	before := ix.watermark
	items, err := ix.src.AllForMemoryIndex(ctx, ix.watermark, ix.batchSize)
	if err != nil {
		return 0, err
	}
	indexed := 0
	// FREEZE discipline (same self-healing pattern as discussionindex/handoffmirror): once a revision
	// FAILS to index, freeze the watermark so the failed item is re-fetched next sweep; later items are
	// still indexed (and remembered in `seen`) but the watermark must not advance PAST the failure. A
	// TEAMLESS item deliberately does NOT freeze — one permanently-teamless item must never wedge the
	// indexer's liveness (every newer item would stall behind it), exactly the tradeoff handoffmirror
	// documents. The cost is honest: once the watermark advances past a teamless item it is not
	// re-fetched in this process (a restart re-sweeps from the cursor and picks it up if it has since
	// gained a team). Best-effort, not best-blocking.
	frozen := false
	for _, w := range items {
		rid := recordID(w)
		if _, done := ix.seen[rid]; done {
			// This exact revision was already indexed (a watermark-boundary tie) — skip re-projecting;
			// advance only if not frozen.
			if !frozen {
				ix.advance(w.ActivityAt, rid)
			}
			continue
		}
		if w.TeamID == "" {
			log.Printf("workitemindex: skip work item %s (no team scope yet; deferring, not freezing the sweep)", w.WorkItemID)
			continue
		}
		if err := ix.index(ctx, w, rid); err != nil {
			log.Printf("workitemindex: skip work item %s (best-effort, will retry): %v", w.WorkItemID, err)
			frozen = true
			continue
		}
		ix.seen[rid] = struct{}{}
		if !frozen {
			ix.advance(w.ActivityAt, rid)
		}
		indexed++
	}
	// Persist the watermark AFTER the batch's writes commit. A crash before this leaves the durable
	// cursor behind the writes, so a restart re-scans the tail and the idempotent revision projection
	// collapses the re-scan to a no-op — never a drop, never a duplicate. Save only when the watermark
	// actually advanced, to avoid churning the cursor row every idle tick.
	if ix.cursor != nil && ix.watermark.After(before) {
		if err := ix.cursor.SaveProjectionCursor(ctx, cursorName, ix.watermark, ix.watermarkID); err != nil {
			// Fail-open: the in-process watermark still advanced, so recall is unaffected; the only cost
			// of a lost save is a bounded idempotent re-scan on the next restart.
			log.Printf("workitemindex: persist cursor failed (best-effort, in-process watermark retained): %v", err)
		}
	}
	return indexed, nil
}

// ensureCursorLoaded loads the durable watermark once, on the first sweep, seeding the in-process
// watermark so the projection resumes from its last committed position. A load error is fail-open: the
// sweep falls back to a zero watermark and re-scans from the beginning, which the idempotent revision
// projection makes safe (no duplicate rows) — only a bounded one-time re-scan.
func (ix *Indexer) ensureCursorLoaded(ctx context.Context) {
	if ix.cursor == nil || ix.cursorLoaded {
		return
	}
	ix.cursorLoaded = true
	c, ok, err := ix.cursor.LoadProjectionCursor(ctx, cursorName)
	if err != nil {
		log.Printf("workitemindex: load cursor failed (best-effort, sweeping from zero): %v", err)
		return
	}
	if !ok {
		return // never saved — first boot; sweep from zero
	}
	ix.watermark = c.LastProjectedAt
	// Seed the seen-set with the boundary record id so the exact revision at the watermark is not
	// re-written (its idempotent write would be a no-op anyway; this just skips the round-trip). The
	// source query is `activity >= watermark`, so any OTHER items sharing that activity timestamp are
	// still re-fetched and either skipped (unchanged ⇒ same id, idempotent) or re-projected (changed).
	if c.LastProjectedID != "" {
		ix.watermarkID = c.LastProjectedID
		ix.seen[c.LastProjectedID] = struct{}{}
	}
}

// advance moves the watermark forward monotonically (never backward), recording the record id at the
// new high-water mark for persistence as tie provenance.
func (ix *Indexer) advance(t time.Time, id string) {
	if t.After(ix.watermark) {
		ix.watermark = t
		ix.watermarkID = id
	}
}

// index projects ONE work-item revision into a provenanced memory write: scoped to the item's
// project/team, kind KindWorkItem, authored by the coord created_by principal (TEXT, carried verbatim
// in provenance — coord has no agent identity column, so agent_id stays nil and is_agent is derived
// false, exactly like the handoff mirror). The content is title + body + comment thread; the embedding
// is computed by the seam embedder. The supersede then soft-retracts the item's earlier revisions so
// exactly one live record per work item remains.
func (ix *Indexer) index(ctx context.Context, w WorkItemIndexable, rid string) error {
	content := indexedContent(w)
	vec, err := ix.embed.Embed(ctx, content)
	if err != nil {
		return fmt.Errorf("embed work item: %w", err)
	}

	prov := memory.NewWorkItemProvenance(w.WorkItemID, w.State, w.CreatedBy, w.CommentCount, w.CreatedAt, w.ActivityAt)

	projectID := w.ProjectID
	_, err = ix.sink.Write(ctx, memory.WriteRequest{
		SquadID:     w.TeamID,
		ProjectID:   &projectID,
		PrincipalID: deriveUUID("principal", w.CreatedBy),
		Kind:        memory.KindWorkItem,
		Content:     content,
		Embedding:   vec,
		Provenance:  prov,
		DedupeID:    &rid,
	})
	if err != nil {
		return fmt.Errorf("write work item record: %w", err)
	}
	if ix.supersede != nil {
		n, serr := ix.supersede.SupersedeWorkItemRecords(ctx, w.TeamID, w.WorkItemID, rid)
		if serr != nil {
			// The new revision is live; only the OLD-rows retract failed. Older revisions surfacing
			// alongside the newest is a staleness blemish, never a correctness failure (both are
			// untrusted recall rows of genuinely committed revisions) — log and move on (best-effort).
			log.Printf("workitemindex: supersede after work item %s failed, older revisions stay live (best-effort): %v", w.WorkItemID, serr)
			return nil
		}
		if n > 0 {
			log.Printf("workitemindex: work item %s superseded %d earlier revision(s)", w.WorkItemID, n)
		}
	}
	return nil
}

// indexedContent composes the searchable text for one work item: the title, then the body, then the
// comment thread. One embedding per item (the item becomes ONE recallable record) — a ticket's title,
// description, and discussion all contribute to whether it surfaces for a semantically related query.
func indexedContent(w WorkItemIndexable) string {
	var b strings.Builder
	b.WriteString(w.Title)
	if w.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(w.Body)
	}
	if w.CommentBodies != "" {
		b.WriteString("\n\n[comments]\n")
		b.WriteString(w.CommentBodies)
	}
	return b.String()
}

// Run drives the sweep on an interval until ctx is cancelled. Errors are logged and the loop continues:
// a transient store/DB error must not tear down the memory service or stall the coord record.
func (ix *Indexer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := ix.Sweep(ctx); err != nil {
				log.Printf("workitemindex: sweep error (best-effort, will retry): %v", err)
			} else if n > 0 {
				log.Printf("workitemindex: indexed %d work item revision(s)", n)
			}
		}
	}
}

// SQLSource is the WorkItemSource over the shared Postgres coord schema. It selects board-authored
// items only (source='board', the ADR-0024b §4.2 allowlist — a dispatch-on-mention thread-run
// (source='discussion') is ephemeral coordination, never a real ticket, and is excluded here exactly as
// it is from the WS-B FTS corpus so the semantic and full-text indexes cover the SAME corpus), joined
// LATERAL to their comment thread.
type SQLSource struct {
	db *sql.DB
}

// NewSQLSource binds the source to the coordination store.
func NewSQLSource(db *sql.DB) *SQLSource {
	return &SQLSource{db: db}
}

// AllForMemoryIndex returns board work items whose last activity (GREATEST(updated_at, latest comment
// created_at)) is at/after `since`, oldest-activity-first, each carrying its own project/team tenancy
// and its aggregated comment thread. A new comment advances the item's activity even though
// coord.comment has no trigger on the parent row, so comment-only changes are re-indexed too. Not an
// agent read path: a trusted server-internal sweep (the outbox-relay posture).
func (s *SQLSource) AllForMemoryIndex(ctx context.Context, since time.Time, limit int) ([]WorkItemIndexable, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
		SELECT w.id::text, w.project_id::text, COALESCE(w.team_id::text, ''),
		       w.title, COALESCE(w.body, ''), w.state, w.created_by, w.created_at,
		       COALESCE(cc.cnt, 0),
		       COALESCE(cc.bodies, ''),
		       GREATEST(w.updated_at, COALESCE(cc.last_at, w.updated_at)) AS activity_at
		  FROM coord.work_item w
		  LEFT JOIN LATERAL (
		      SELECT count(*) AS cnt,
		             max(c.created_at) AS last_at,
		             string_agg(c.author_principal || ': ' || c.body, E'\n' ORDER BY c.created_at) AS bodies
		        FROM coord.comment c
		       WHERE c.work_item_id = w.id
		  ) cc ON true
		 WHERE w.source = 'board'
		   AND GREATEST(w.updated_at, COALESCE(cc.last_at, w.updated_at)) >= $1
		 ORDER BY activity_at ASC, w.id ASC
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkItemIndexable
	for rows.Next() {
		var w WorkItemIndexable
		if err := rows.Scan(&w.WorkItemID, &w.ProjectID, &w.TeamID,
			&w.Title, &w.Body, &w.State, &w.CreatedBy, &w.CreatedAt,
			&w.CommentCount, &w.CommentBodies, &w.ActivityAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ensure the concrete source satisfies the seam at compile time.
var _ WorkItemSource = (*SQLSource)(nil)
