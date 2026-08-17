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
// are never double-indexed within a process. A persistent cursor (survive restart without re-projecting)
// is a fast-follow behind this same seam; re-projection is non-destructive (deterministic ids/content).
type Indexer struct {
	src   MessageSource
	sink  memory.Backend
	embed memory.Embedder

	watermark time.Time
	seen      map[uuid.UUID]struct{}
	batchSize int
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

// Sweep projects one batch of not-yet-indexed messages into the memory index and returns how many were
// newly indexed. It is best-effort: a single message that fails to embed or write is logged and skipped
// (it will be retried on the next sweep), never aborting the batch — the room is never blocked (AC5).
func (ix *Indexer) Sweep(ctx context.Context) (int, error) {
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
				ix.advance(m.CreatedAt)
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
			ix.advance(m.CreatedAt)
		}
		indexed++
	}
	return indexed, nil
}

// advance moves the watermark forward monotonically (never backward).
func (ix *Indexer) advance(t time.Time) {
	if t.After(ix.watermark) {
		ix.watermark = t
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
	req := memory.WriteRequest{
		SquadID:     m.TeamID.String(),
		ProjectID:   &projectID,
		PrincipalID: deriveUUID("principal", m.AuthorPrincipal),
		Kind:        memory.KindDiscussion,
		Content:     m.Body,
		Embedding:   vec,
		Provenance:  prov,
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
