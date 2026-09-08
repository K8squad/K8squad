// Package discussionindex is the 10.2 bridge: it projects live discussion.message rows into the
// ksquad-memory pgvector index (behind the Backend seam), so the room becomes RECALLABLE as
// distrusted, attributed, Team-scoped knowledge (arch §7.5, §7.6, ADR-019). It imports both the
// discussion store (source) and the memory backend (sink); neither imports the other, so this package
// is the ONLY coupling point and the two schemas stay independent.
//
// Posture (AC5, §17.4 — the outbox-relay posture): indexing is best-effort and post-commit. The sweep
// runs on the memory service out of band, pulls messages the room has ALREADY committed, and can only
// ever MIRROR the server-stamped provenance triple — it invents no author and trusts no client-supplied
// field. A room write or Run therefore never waits on, and is never failed by, the indexer.
package discussionindex

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/memory"
)

// MessageSource is the discussion side of the bridge (satisfied by *discussion.Store). AllForMemoryIndex
// returns live messages across every room since a watermark, each carrying its own tenant scope.
type MessageSource interface {
	AllForMemoryIndex(ctx context.Context, since time.Time, limit int) ([]discussion.MemoryIndexable, error)
}

// CursorStore is the durable-watermark seam (satisfied by *memory.PgVectorStore). It persists the
// projection's position so a restart resumes from the last committed watermark rather than re-scanning
// from zero or skipping a window (Story J-C AC1). Optional: a nil store keeps the pre-J-C in-process
// watermark only — used by unit tests and any sink without a cursor table.
type CursorStore interface {
	LoadProjectionCursor(ctx context.Context, name string) (memory.ProjectionCursor, bool, error)
	SaveProjectionCursor(ctx context.Context, name string, at time.Time, id string) error
}

// cursorName is the projection_cursor key for the discussion→memory indexer.
const cursorName = "discussion"

// principalNamespace is a fixed UUIDv5 namespace: the memory substrate columns (principal_id/agent_id/
// run_id) are uuid NOT NULL, but the discussion provenance is TEXT ("alice@corp", "agent:coordinator").
// We derive a STABLE uuid from each text identity for the substrate columns; the honest text triple is
// carried verbatim in `provenance` and is what the read envelope surfaces (§7.3.2). Same text ⇒ same
// uuid, so re-projecting a message is idempotent in the substrate columns too.
var principalNamespace = uuid.MustParse("6b1e5b1e-2c9a-5e7d-9f3a-10b2c3d4e5f6")

// deriveUUID maps a discussion text identity to a deterministic uuid for a memory substrate column.
func deriveUUID(prefix, text string) string {
	return uuid.NewSHA1(principalNamespace, []byte(prefix+":"+text)).String()
}

// Indexer projects committed discussion messages into the memory pgvector index. It keeps an in-process
// watermark (max created_at indexed) plus a seen-set of message ids, so ties at the watermark boundary
// are never double-indexed within a process. When a CursorStore is attached (WithCursor), the watermark
// also survives a restart: it is loaded once on the first sweep and persisted after each advancing
// sweep, so a crash/redeploy resumes from the last committed position (Story J-C AC1). Re-projection is
// non-destructive — the deterministic record id (deriveUUID) makes the memory write idempotent (AC2).
type Indexer struct {
	src   MessageSource
	sink  memory.Backend
	embed memory.Embedder

	cursor       CursorStore // nil ⇒ in-process watermark only (unit tests / sink without a cursor table)
	cursorLoaded bool

	watermark   time.Time
	watermarkID uuid.UUID // source id at the watermark — persisted as tie provenance
	seen        map[uuid.UUID]struct{}
	batchSize   int
}

// NewIndexer wires the bridge. batchSize<=0 defaults to 200 (the store's cap).
func NewIndexer(src MessageSource, sink memory.Backend, embed memory.Embedder, batchSize int) *Indexer {
	if batchSize <= 0 {
		batchSize = 200
	}
	return &Indexer{
		src:       src,
		sink:      sink,
		embed:     embed,
		seen:      make(map[uuid.UUID]struct{}),
		batchSize: batchSize,
	}
}

// WithCursor attaches a durable watermark, making the projection restart-surviving (Story J-C AC1). The
// persisted cursor is loaded lazily on the first Sweep and advanced after each sweep that moves the
// watermark. A nil store is a no-op (in-process watermark only). Returns the indexer for chaining.
func (ix *Indexer) WithCursor(store CursorStore) *Indexer {
	ix.cursor = store
	return ix
}

// Sweep projects one batch of not-yet-indexed messages into the memory index and returns how many were
// newly indexed. It is best-effort: a single message that fails to embed or write is logged and skipped
// (it will be retried on the next sweep), never aborting the batch — the room is never blocked (AC5).
func (ix *Indexer) Sweep(ctx context.Context) (int, error) {
	ix.ensureCursorLoaded(ctx)

	before := ix.watermark
	msgs, err := ix.src.AllForMemoryIndex(ctx, ix.watermark, ix.batchSize)
	if err != nil {
		return 0, err
	}
	indexed := 0
	// Once a row in this batch fails, FREEZE the watermark: later rows are still indexed (and
	// remembered in `seen` so a retry never double-indexes them), but the watermark must not advance
	// PAST the failed row's timestamp, or the failure would be skipped forever. The frozen watermark
	// re-fetches the failed row on the next sweep — self-healing best-effort (AC5).
	frozen := false
	for _, m := range msgs {
		if _, done := ix.seen[m.MessageID]; done {
			// Already indexed (a watermark-boundary tie) — skip re-projecting; advance only if not frozen.
			if !frozen {
				ix.advance(m.CreatedAt, m.MessageID)
			}
			continue
		}
		if err := ix.index(ctx, m); err != nil {
			log.Printf("discussionindex: skip message %s (best-effort, will retry): %v", m.MessageID, err)
			frozen = true
			continue
		}
		ix.seen[m.MessageID] = struct{}{}
		if !frozen {
			ix.advance(m.CreatedAt, m.MessageID)
		}
		indexed++
	}
	// Persist the watermark AFTER the batch's writes are committed (AC1/AC2). A crash before this saves
	// leaves the durable cursor behind the writes, so a restart re-scans the tail and the idempotent
	// projection collapses the re-scan to a no-op — never a drop, never a duplicate. Only save when the
	// watermark actually advanced this sweep, to avoid churning the cursor row every idle tick.
	if ix.cursor != nil && ix.watermark.After(before) {
		if err := ix.cursor.SaveProjectionCursor(ctx, cursorName, ix.watermark, ix.watermarkID.String()); err != nil {
			// Fail-open (AC3): the in-process watermark still advanced, so recall is unaffected; the only
			// cost of a lost save is a bounded idempotent re-scan on the next restart.
			log.Printf("discussionindex: persist cursor failed (best-effort, in-process watermark retained): %v", err)
		}
	}
	return indexed, nil
}

// ensureCursorLoaded loads the durable watermark once, on the first sweep, seeding the in-process
// watermark so the projection resumes from its last committed position (AC1). A load error is fail-open
// (AC3): the sweep falls back to a zero watermark and re-scans from the beginning, which the idempotent
// projection makes safe (no duplicate rows) — only a bounded one-time re-scan.
func (ix *Indexer) ensureCursorLoaded(ctx context.Context) {
	if ix.cursor == nil || ix.cursorLoaded {
		return
	}
	ix.cursorLoaded = true
	c, ok, err := ix.cursor.LoadProjectionCursor(ctx, cursorName)
	if err != nil {
		log.Printf("discussionindex: load cursor failed (best-effort, sweeping from zero): %v", err)
		return
	}
	if !ok {
		return // never saved — first boot; sweep from zero
	}
	ix.watermark = c.LastProjectedAt
	// Seed the seen-set with the boundary id so the exact watermark row is not even re-written (its
	// idempotent write would be a no-op anyway; this just skips the round-trip). The source query is
	// `created_at >= watermark`, so any OTHER rows sharing that timestamp are still re-fetched and
	// deduplicated by the idempotent write.
	if id, perr := uuid.Parse(c.LastProjectedID); perr == nil {
		ix.watermarkID = id
		ix.seen[id] = struct{}{}
	}
}

// advance moves the watermark forward monotonically (never backward), recording the source id at the
// new high-water mark for persistence as tie provenance.
func (ix *Indexer) advance(t time.Time, id uuid.UUID) {
	if t.After(ix.watermark) {
		ix.watermark = t
		ix.watermarkID = id
	}
}

// index projects ONE message into a memory write. The provenance triple is mirrored VERBATIM from the
// 10.1 server-stamped columns (principal/agent/run) into `provenance`; the uuid substrate columns get
// deterministic derivations. Kind is the constant "discussion" so the read tool can narrow on it. The
// embedding is computed by the seam embedder (§7.1).
func (ix *Indexer) index(ctx context.Context, m discussion.MemoryIndexable) error {
	vec, err := ix.embed.Embed(ctx, m.Body)
	if err != nil {
		return err
	}

	prov := memory.NewDiscussionProvenance(
		m.MessageID.String(), m.ThreadID.String(), m.AuthorPrincipal,
		m.AuthorAgentID, m.AuthorRunID, m.CreatedAt)

	projectID := m.ProjectID.String()
	// The record id is DERIVED deterministically from the message id (same UUIDv5 namespace as the
	// substrate columns), so re-projecting a message on a crash-replay upserts the SAME row rather than
	// duplicating it — the AC2 exactly-once-into-recall guarantee, paired with the store's ON CONFLICT.
	recordID := deriveUUID("discussion-record", m.MessageID.String())
	req := memory.WriteRequest{
		SquadID:     m.TeamID.String(),
		ProjectID:   &projectID,
		PrincipalID: deriveUUID("principal", m.AuthorPrincipal),
		Kind:        memory.KindDiscussion,
		Content:     m.Body,
		Embedding:   vec,
		Provenance:  prov,
		DedupeID:    &recordID,
	}
	if m.AuthorAgentID != nil {
		a := deriveUUID("agent", *m.AuthorAgentID)
		req.AgentID = &a
	}
	if m.AuthorRunID != nil {
		r := deriveUUID("run", *m.AuthorRunID)
		req.RunID = &r
	}
	_, err = ix.sink.Write(ctx, req)
	return err
}

// Run drives the sweep on an interval until ctx is cancelled. Errors are logged and the loop continues:
// a transient store/DB error must not tear down the memory service or stall the room (best-effort, AC5).
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
				log.Printf("discussionindex: sweep error (best-effort, will retry): %v", err)
			} else if n > 0 {
				log.Printf("discussionindex: indexed %d discussion message(s)", n)
			}
		}
	}
}

// ensure the concrete discussion store satisfies the source seam at compile time.
var _ MessageSource = (*discussion.Store)(nil)
