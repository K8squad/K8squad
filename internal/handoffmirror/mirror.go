// Package handoffmirror is the §8.5/§6.6 mirror bridge: it projects committed
// structured handoff artifacts (Story 2.8 — the kind "handoff" coord.artifact row
// and its append-only coord.audit_log provenance/content row) into the
// ksquad-memory pgvector index, so a completed Run's handoff becomes RECALLABLE
// by the NEXT Run as distrusted, attributed, Team-scoped knowledge (Story 6.6,
// arch §8.5, ADR-028). It imports the coord record (source constants) and the
// memory backend (sink); pkg/coord does not import memory and memory does not
// import pkg/coord — this package is the ONLY coupling point.
//
// Posture (AC6, §17.4 — the outbox-relay posture, same as discussionindex 10.2):
// mirroring is best-effort and post-commit. The sweep runs on the memory service
// out of band, pulls handoffs the coord record has ALREADY committed, and can
// only ever MIRROR the server-stamped audit row — it invents no author and
// trusts no client-supplied field. A handoff write therefore never waits on, and
// is never failed by, the mirror; a memory outage never rolls back the artifact.
//
// No-custody (AC4, the no-P2P lock a sixth time): the mirror is a plain
// provenanced memory write and recall of it is a plain untrusted read. Nothing
// here touches coord.claim or coord.work_item, and no custody/grant field exists
// on the mirror — recalling a handoff confers no custody; the fence stays the
// sole discriminator (§6.2/§6.3).
package handoffmirror

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/coord"
)

// MirrorSourceRow is one committed handoff publication ready to mirror: the
// audit row (id, work item, run, principal, fence, payload, created_at) joined
// with the live artifact registration (uri, sha256) and the work item's tenancy
// (project, team). The uri join means only the audit row the CURRENT artifact
// registration addresses is a source row — a republish surfaces as a NEW row
// (new audit id) and the older audit row drops out of the source naturally.
type MirrorSourceRow struct {
	AuditID    int64
	WorkItemID string
	RunID      string
	Principal  string
	FenceToken *int64
	Payload    string // the canonical HandoffDoc jsonb bytes, verbatim (content = bytes at rest)
	URI        string
	SHA256     string
	CreatedAt  time.Time
	ProjectID  string
	TeamID     string // "" ⇒ the work item has no team scope yet — unmirrorable (see Sweep)
}

// HandoffSource is the coord side of the bridge. AllForMemoryMirror returns the
// live handoff publications committed at/after `since`, oldest-first, each
// carrying its own tenancy. Satisfied by SQLSource over the shared Postgres.
type HandoffSource interface {
	AllForMemoryMirror(ctx context.Context, since time.Time, limit int) ([]MirrorSourceRow, error)
}

// Superseder is the optional §6.6 republish-retire companion on the store side
// (satisfied by *memory.PgVectorStore). When a Run REPUBLISHES its handoff, the
// coord artifact upserts in place (one live artifact per (work_item, run)) — the
// mirror must likewise keep exactly ONE live mirror per pair, soft-retracting
// earlier mirrors so recall surfaces only the newest publication.
type Superseder interface {
	SupersedeHandoffMirrors(ctx context.Context, squadID, workItemID, runID, keepID string) (int64, error)
}

// principalNamespace is the SAME fixed UUIDv5 namespace discussionindex derives
// substrate uuids with: a text principal maps to the SAME deterministic uuid in
// both bridges, so a principal's memory rows join on the substrate columns
// regardless of which bridge wrote them. The honest TEXT attribution still rides
// verbatim in `provenance` (provenance in = provenance out).
var principalNamespace = uuid.MustParse("6b1e5b1e-2c9a-5e7d-9f3a-10b2c3d4e5f6")

// deriveUUID maps a coord text identity to a deterministic uuid for a memory
// substrate column (same derivation as the discussion bridge).
func deriveUUID(prefix, text string) string {
	return uuid.NewSHA1(principalNamespace, []byte(prefix+":"+text)).String()
}

// Mirror projects committed handoff publications into the memory pgvector
// index. It keeps an in-process watermark (max audit created_at mirrored) plus a
// seen-set of audit ids, so boundary ties and re-sweeps never double-mirror
// within a process. Re-projection is non-destructive (deterministic provenance;
// a duplicate write is a duplicate row the supersede path collapses, never a
// correctness failure).
type Mirror struct {
	src       HandoffSource
	sink      memory.Backend
	embed     memory.Embedder
	supersede Superseder // nil ⇒ republish-retire disabled (tests/small sinks)

	watermark time.Time
	seen      map[int64]struct{}
	batchSize int
}

// NewMirror wires the bridge. batchSize<=0 defaults to 100. superseder may be
// nil (republish-retire simply disabled); production wiring passes the
// *memory.PgVectorStore, which implements it.
func NewMirror(src HandoffSource, sink memory.Backend, embed memory.Embedder, superseder Superseder, batchSize int) *Mirror {
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Mirror{
		src:       src,
		sink:      sink,
		embed:     embed,
		supersede: superseder,
		seen:      make(map[int64]struct{}),
		batchSize: batchSize,
	}
}

// Sweep projects one batch of not-yet-mirrored handoff publications into memory
// and returns how many were newly mirrored. Best-effort (AC6): a row that fails
// to embed or write is logged and skipped (retried on the next sweep), never
// aborting the batch — the committed handoff is never blocked or rolled back.
func (m *Mirror) Sweep(ctx context.Context) (int, error) {
	rows, err := m.src.AllForMemoryMirror(ctx, m.watermark, m.batchSize)
	if err != nil {
		return 0, err
	}
	mirrored := 0
	// Once a row in this batch fails, FREEZE the watermark (same self-healing
	// discipline as the discussion indexer): later rows still mirror, but the
	// watermark must not advance PAST the failed row or the failure is skipped
	// forever; the frozen watermark re-fetches it on the next sweep.
	frozen := false
	for _, r := range rows {
		if _, done := m.seen[r.AuditID]; done {
			if !frozen {
				m.advance(r.CreatedAt)
			}
			continue
		}
		if r.TeamID == "" {
			// No team scope on the work item yet (§6.1 team is inherited and may
			// be unset): a memory record's squad_id is NOT NULL, and mirroring to
			// some default scope would FORGE tenancy. Skip — best-effort. The
			// row stays unmirrored (and re-fetched) until it gains a team.
			log.Printf("handoffmirror: skip audit %d (work item %s has no team scope; cannot mirror without forging tenancy)",
				r.AuditID, r.WorkItemID)
			frozen = true
			continue
		}
		if err := m.mirror(ctx, r); err != nil {
			log.Printf("handoffmirror: skip audit %d (best-effort, will retry): %v", r.AuditID, err)
			frozen = true
			continue
		}
		m.seen[r.AuditID] = struct{}{}
		if !frozen {
			m.advance(r.CreatedAt)
		}
		mirrored++
	}
	return mirrored, nil
}

// advance moves the watermark forward monotonically (never backward).
func (m *Mirror) advance(t time.Time) {
	if t.After(m.watermark) {
		m.watermark = t
	}
}

// mirror projects ONE handoff publication into a provenanced memory write
// (AC3): scoped to the work item's project/team, kind handoff-mirror, authored
// by the coord principal (TEXT, carried verbatim in provenance — coord has no
// agent identity column, so agent_id stays honestly nil and is_agent is derived
// false; the principal text IS the attribution). The content is the canonical
// HandoffDoc bytes at rest; the embedding is computed by the seam embedder. The
// supersede then soft-retracts earlier mirrors of the same (work item, run) so
// exactly one live mirror per publication pair remains (§6.4 republish).
func (m *Mirror) mirror(ctx context.Context, r MirrorSourceRow) error {
	vec, err := m.embed.Embed(ctx, r.Payload)
	if err != nil {
		return fmt.Errorf("embed handoff doc: %w", err)
	}
	// agentID stays nil: coord's holder principal is free TEXT ("agent-a",
	// "alice@corp"); deriving an agent identity it never stamped would launder
	// authorship. The envelope surfaces the principal verbatim.
	prov := memory.NewHandoffProvenance(
		r.URI, r.SHA256, r.WorkItemID, r.RunID, r.AuditID, r.FenceToken,
		r.Principal, nil, r.CreatedAt)

	projectID := r.ProjectID
	runUUID := deriveUUID("run", r.RunID)
	rec, err := m.sink.Write(ctx, memory.WriteRequest{
		SquadID:     r.TeamID,
		ProjectID:   &projectID,
		PrincipalID: deriveUUID("principal", r.Principal),
		RunID:       &runUUID,
		Kind:        memory.KindHandoffMirror,
		Content:     r.Payload,
		Embedding:   vec,
		Provenance:  prov,
	})
	if err != nil {
		return fmt.Errorf("write handoff mirror: %w", err)
	}
	if m.supersede != nil {
		n, serr := m.supersede.SupersedeHandoffMirrors(ctx, r.TeamID, r.WorkItemID, r.RunID, rec.ID)
		if serr != nil {
			// The new mirror is live; only the OLD-rows retract failed. Older
			// mirrors surfacing alongside the newest is a staleness blemish,
			// never a correctness failure (both are untrusted recall rows of
			// genuinely committed publications) — log and move on (best-effort).
			log.Printf("handoffmirror: supersede after audit %d failed, older mirrors stay live (best-effort): %v",
				r.AuditID, serr)
			return nil
		}
		if n > 0 {
			log.Printf("handoffmirror: audit %d superseded %d earlier mirror(s) of work item %s run %s",
				r.AuditID, n, r.WorkItemID, r.RunID)
		}
	}
	return nil
}

// Run drives the sweep on an interval until ctx is cancelled. Errors are logged
// and the loop continues: a transient store/DB error must not tear down the
// memory service or stall the coord record (best-effort, AC6).
func (m *Mirror) Run(ctx context.Context, interval time.Duration) {
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
			if n, err := m.Sweep(ctx); err != nil {
				log.Printf("handoffmirror: sweep error (best-effort, will retry): %v", err)
			} else if n > 0 {
				log.Printf("handoffmirror: mirrored %d handoff publication(s)", n)
			}
		}
	}
}

// SQLSource is the HandoffSource over the shared Postgres coord schema. It
// selects ONLY audit rows addressed by the LIVE artifact registration
// (art.uri = coord+audit://<audit id>, kind "handoff"): the 2.8 upsert keeps one
// artifact row per (work_item, run, kind) whose uri moves to the newest audit
// row on republish, so the source emits exactly the current publication per
// pair and a republish arrives here as a NEW row (new audit id).
type SQLSource struct {
	db *sql.DB
}

// NewSQLSource binds the source to the coordination store.
func NewSQLSource(db *sql.DB) *SQLSource {
	return &SQLSource{db: db}
}

// AllForMemoryMirror returns live handoff publications committed at/after
// `since`, oldest-first, each carrying its own project/team tenancy (joined
// from the work item — the audit row itself is tenancy-free). Not an agent read
// path: a trusted server-internal sweep (§17.4 outbox-relay posture).
func (s *SQLSource) AllForMemoryMirror(ctx context.Context, since time.Time, limit int) ([]MirrorSourceRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
		SELECT al.id, al.work_item_id::text, al.run_id::text, al.principal,
		       al.fence_token, al.payload::text, art.uri, art.sha256,
		       al.created_at, wi.project_id::text, coalesce(wi.team_id::text, '')
		  FROM coord.audit_log al
		  JOIN coord.artifact art
		    ON art.kind = $1
		   AND art.work_item_id = al.work_item_id
		   AND art.run_id = al.run_id
		   AND art.uri = $2 || al.id::text
		  JOIN coord.work_item wi ON wi.id = al.work_item_id
		 WHERE al.event_type = 'artifact_registered'
		   AND al.created_at >= $3
		 ORDER BY al.created_at ASC
		 LIMIT $4`
	rows, err := s.db.QueryContext(ctx, q, coord.HandoffKind, coord.AuditHandoffURI, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MirrorSourceRow
	for rows.Next() {
		var r MirrorSourceRow
		if err := rows.Scan(&r.AuditID, &r.WorkItemID, &r.RunID, &r.Principal,
			&r.FenceToken, &r.Payload, &r.URI, &r.SHA256,
			&r.CreatedAt, &r.ProjectID, &r.TeamID); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(r.URI, coord.AuditHandoffURI) {
			return nil, fmt.Errorf("handoffmirror: artifact uri %q not a %s address (schema drift?)", r.URI, coord.AuditHandoffURI)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ensure the concrete source satisfies the seam at compile time.
var _ HandoffSource = (*SQLSource)(nil)
