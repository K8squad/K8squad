// prodhandoff.go — the §2.8/§6.5 STRUCTURED HANDOFF ARTIFACT writer BOUND TO
// THE PRODUCTION SCHEMA (Story 2.8 / ISI-2525). Where prodclaim.go (2.2) binds
// the §6.2 claim and proddispatch.go (2.9) binds the coordinator loop that
// CONSUMES handoffs, this file owns the WRITE side: when a Run completes or
// pauses, its agent publishes what the next actor needs to know as ONE
// provenance-tagged artifact row in the coordination record —
//
//	{did, decisions, next, blockers, findings, recommended_next,
//	 artifacts_for_downstream}                                   (Story 2.8)
//
// # Channel (FR-B3): the record, and nothing else
//
// The handoff rides the A2A artifact channel of §6.5 — coord.artifact
// (content-addressed registration, upsert-keyed per §6.4) plus a
// coord.audit_log row (append-only provenance: principal, run, fence). There
// is NO message table and NO new surface: the only path B's words take to A is
// a row in the shared record (Arch §6.1/§8.4, ADR-028), which is exactly what
// ProdDispatcher.ReadHandoff (2.9) reads back.
//
// # Advisory ONLY — custody stays fenced (Arch §8.5)
//
// A handoff informs; it never decides. WriteHandoff performs NO custody
// mutation: coord.claim is not touched (no fence bump, no holder change, no
// lease change) and coord.work_item is not touched (no lane change). Custody
// moves only through the fenced release → re-dispatch → claim path (§6.2/§6.3,
// ADR-028); recommended_next in the doc is a DRAFT the coordinator may adopt,
// reorder or discard (2.9's P2).
//
// The WRITE itself is custody-GATED, however: only the CURRENT live holder —
// same principal, same run, same fence, unexpired lease, evaluated against
// clock_timestamp() at statement time — may register the handoff. A fenced
// zombie or a lease-lapsed former holder cannot poison the next claimant's
// context. This is the same guard family as Complete/DispatchOnce (§6.2/§6.4).
//
// # Content binding (v1): the audit row IS the object store
//
// coord.artifact stores a POINTER (uri, sha256), not content. v1 has no
// object store wired into the spine, so the handoff doc's canonical bytes live
// in the audit row's jsonb payload — durable, append-only, and already
// provenanced — and the artifact uri addresses them:
//
//	coord+audit://<audit_log id>
//
// AuditHandoffContent is the resolver for that scheme (it verifies the
// content hash against the artifact row's sha256 on every read, fail-closed
// on tamper or pointer drift). It is the "2.8/8.x wiring" ProdDispatcher's
// ArtifactContent hook expects; an 8.x object-store binding swaps the resolver,
// not the writer. The bytes at the uri are the HandoffDoc JSON itself, so any
// reader unmarshals them directly.
//
// # Provenanced memory mirror (Arch §6.6)
//
// The audit row IS the mirror source: its columns (work_item_id, run_id,
// principal, fence_token, created_at) plus its payload (the doc) form a
// self-contained, provenance-tagged record Epic 6 can mirror to memory without
// re-deriving or joining anything. Nothing here writes memory itself — memory
// is not a handoff channel (FR-B3); the mirror is a separate, §6.6-scoped
// consumer of this row.
//
// # Idempotency (§6.4) and republish semantics
//
// One handoff artifact per (work_item, run): the shipped UNIQUE
// (work_item_id, run_id, kind) key turns a re-drive into an in-place
// republish — same content is a no-op hash, changed content updates uri/sha256
// — never a duplicate row. The DO UPDATE arm is custody-gated by the same
// EXISTS predicate as the insert arm, so a since-fenced run cannot republish.
// Audit rows append per WRITE (each attempt is an event, append-only by
// trigger); the artifact row is the single canonical doc.
//
// # Type ownership note (supersedes the 2.9 WIP preview)
//
// HandoffKind, HandoffDoc, DraftWorkItem and ArtifactRef are THE canonical
// contract types, owned by this story; their JSON keys are exactly what
// proddispatch.go's read side unmarshals. The 2.9 work-in-progress carried a
// three-field preview of HandoffDoc (findings/recommended_next/
// artifacts_for_downstream) — on rebase it should delete its preview
// declarations and consume these; the shared fields are shape- and
// key-identical, and HandoffDoc carries the remaining four Story 2.8 fields
// (did, decisions, next, blockers) the preview omitted.
package coord

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// HandoffKind is the pinned coord.artifact kind of the structured handoff
// (Story 2.8 contract, consumed by 2.9's ReadHandoff).
const HandoffKind = "handoff"

// AuditHandoffURI is the v1 content-addressing scheme: the artifact uri names
// the audit_log row whose payload holds the canonical HandoffDoc bytes.
const AuditHandoffURI = "coord+audit://"

// ErrNotHandoffCustodian refuses an advisory write from anyone but the current
// live fence-matching holder. Refusal, not error-retry: the caller either
// holds custody (and should re-derive its fence via the §6.2 claim path) or it
// does not (and must not publish context for an item it no longer runs).
var ErrNotHandoffCustodian = errors.New(
	"coord: handoff refused — caller is not the live fence-matching holder " +
		"(advisory write, custody stays fenced §8.5)")

// DraftWorkItem is a proposed work item — content only, never a dispatch. The
// handing-off run's recommended_next entries and the coordinator's candidate
// list share the shape; WHO dispatches is 2.9's concern, not the shape's.
type DraftWorkItem struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// ArtifactRef mirrors the coord.artifact columns the record yields
// (§6.1/§6.4): what kind of output, where its canonical bytes live, and the
// digest that verifies them.
type ArtifactRef struct {
	Kind   string `json:"kind"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

// HandoffDoc is the structured, ADVISORY-ONLY handoff payload Story 2.8 pins —
// the seven fields of the story, verbatim:
//
//	did, decisions, next, blockers — what happened on this item
//	findings — what the Run learned, as prose for the next actor
//	recommended_next — DRAFT work items the coordinator may adopt (2.9 P2)
//	artifacts_for_downstream — registered outputs downstream work should consume
//
// CANONICAL FORM NOTE: the artifact content is the doc AS STORED — the
// jsonb-normalized serialization coord.audit_log.payload returns (jsonb
// canonicalizes key order and whitespace), NOT the raw json.Marshal bytes.
// WriteHandoff hashes and registers exactly the bytes the resolver will hand
// back (it reads the stored form back inside the write transaction), so the
// same doc always yields the same digest and a republish of identical content
// is an observable no-op.
type HandoffDoc struct {
	Did                    []string        `json:"did"`
	Decisions              []string        `json:"decisions"`
	Next                   []string        `json:"next"`
	Blockers               []string        `json:"blockers"`
	Findings               string          `json:"findings"`
	RecommendedNext        []DraftWorkItem `json:"recommended_next"`
	ArtifactsForDownstream []ArtifactRef   `json:"artifacts_for_downstream"`
}

// HandoffWriteResult reports what the record now holds.
type HandoffWriteResult struct {
	// AuditLogID is the id of the append-only §6.5 audit row this write
	// appended (the provenance record and — v1 — the content store).
	AuditLogID int64
	// URI addresses the canonical HandoffDoc bytes (AuditHandoffURI scheme).
	URI string
	// SHA256 is the digest of the canonical doc bytes; equal to the
	// coord.artifact row's sha256 column.
	SHA256 string
}

// ProdHandoffWriter registers structured handoff artifacts against the shipped
// coord schema (db/migrations/0001_coord_schema.sql). It holds no mutable Go
// state beyond the *sql.DB and its pinned statements, so it is safe for
// concurrent use by many goroutines (each write opens its own transaction).
type ProdHandoffWriter struct {
	db    *sql.DB
	audit string
	art   string
}

// NewProdHandoffWriter binds the handoff writer to db.
func NewProdHandoffWriter(db *sql.DB) (*ProdHandoffWriter, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdHandoffWriter: nil db")
	}
	return &ProdHandoffWriter{
		db: db,
		// (1) AUDIT — the §6.5 provenance row, GUARDED by the live-custody
		// predicate: the INSERT ... SELECT finds no source claim row (and so
		// inserts nothing) unless this caller is the current holder at this
		// fence under an unexpired lease, re-evaluated against
		// clock_timestamp() at statement time. The payload column doubles as
		// the v1 content store the artifact uri addresses; RETURNING reads the
		// STORED (jsonb-normalized) form back so the digest registered in (2)
		// is computed over exactly the bytes the resolver will return — jsonb
		// canonicalizes key order/whitespace, so hashing the Go-side marshal
		// instead would never match the bytes at rest.
		audit: `
			INSERT INTO coord.audit_log
			       (work_item_id, run_id, event_type, principal,
			        initiated_by_user_id, fence_token, payload)
			SELECT $1::uuid, $2::uuid, 'artifact_registered', $3,
			       $4::uuid, $5, $6::jsonb
			  FROM coord.claim
			 WHERE work_item_id = $1::uuid
			   AND holder_principal = $3
			   AND run_id = $2::uuid
			   AND fence_token = $5
			   AND lease_expires_at > clock_timestamp()
			RETURNING id, payload::text`,
		// (2) ARTIFACT — the §6.4 upsert-keyed registration, guarded on BOTH
		// arms: the insert arm's SELECT source is the same live-custody
		// predicate; the ON CONFLICT republish arm carries the predicate in
		// its DO UPDATE ... WHERE, so a since-fenced run cannot update a row
		// it legitimately wrote while live. Re-drive of the same content
		// rewrites identical uri/sha256 (observable no-op); changed content
		// republishes in place — never a duplicate (shipped UNIQUE
		// (work_item_id, run_id, kind)).
		art: `
			INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
			SELECT $1::uuid, $2::uuid, $3, $4, $5
			  FROM coord.claim
			 WHERE work_item_id = $1::uuid
			   AND holder_principal = $6
			   AND run_id = $2::uuid
			   AND fence_token = $7
			   AND lease_expires_at > clock_timestamp()
			ON CONFLICT (work_item_id, run_id, kind) DO UPDATE
			   SET uri = EXCLUDED.uri, sha256 = EXCLUDED.sha256
			 WHERE EXISTS (SELECT 1 FROM coord.claim c
			                WHERE c.work_item_id = $1::uuid
			                  AND c.holder_principal = $6
			                  AND c.run_id = $2::uuid
			                  AND c.fence_token = $7
			                  AND c.lease_expires_at > clock_timestamp())`,
	}, nil
}

// WriteHandoff registers the structured handoff doc for (itemID, runID) as ONE
// transaction: custody-gated §6.5 audit append (provenance + canonical
// content) → custody-gated §6.4 artifact upsert pointing at it. Advisory
// ONLY: neither coord.claim nor coord.work_item is written — custody stays
// fenced release→re-dispatch→claim (§8.5/ADR-028).
//
// Call this while custody is certainly live — i.e. BEFORE the §6.3 Complete /
// release path runs (after a release the fence has advanced and the write is
// correctly refused). initiatedByUserID may be empty (recorded as NULL; the
// §12.4 control-plane stamp is apiserver wiring).
//
// Semantics:
//
//   - (result, nil): the record now holds one kind "handoff" artifact for this
//     (item, run), addressed by result.URI and digest-verified by
//     result.SHA256, with the audit row appended in the same transaction.
//   - (zero, ErrNotHandoffCustodian): refused — the caller is not the current
//     live fence-matching holder. Nothing was written (transaction rolled
//     back).
//   - (zero, err): infrastructure failure; nothing was written.
func (w *ProdHandoffWriter) WriteHandoff(ctx context.Context, itemID, principal, runID, initiatedByUserID string, fence int64, doc HandoffDoc) (HandoffWriteResult, error) {
	if itemID == "" || principal == "" || runID == "" {
		return HandoffWriteResult{}, fmt.Errorf(
			"coord.ProdHandoffWriter.WriteHandoff: itemID, principal and runID are required (got itemID=%q principal=%q runID=%q)",
			itemID, principal, runID)
	}

	body, err := json.Marshal(doc)
	if err != nil {
		return HandoffWriteResult{}, fmt.Errorf("coord.ProdHandoffWriter.WriteHandoff: marshal doc: %w", err)
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return HandoffWriteResult{}, fmt.Errorf("coord.ProdHandoffWriter.WriteHandoff: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) guarded audit append — also the content store the uri addresses.
	// The digest is computed over the STORED jsonb form RETURNING hands back,
	// not the transport bytes: those are the bytes at rest, and the only form
	// the resolver can (and must) reproduce.
	var initiator any
	if initiatedByUserID != "" {
		initiator = initiatedByUserID
	}
	var auditID int64
	var canonical []byte
	switch err := tx.QueryRowContext(ctx, w.audit,
		itemID, runID, principal, initiator, fence, body,
	).Scan(&auditID, &canonical); {
	case errors.Is(err, sql.ErrNoRows):
		// The custody predicate found no live fence-matching claim row.
		return HandoffWriteResult{}, ErrNotHandoffCustodian
	case err != nil:
		return HandoffWriteResult{}, fmt.Errorf("coord.ProdHandoffWriter.WriteHandoff: audit: %w", err)
	}
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])

	// (2) guarded artifact registration, atomic with the audit append.
	uri := fmt.Sprintf("%s%d", AuditHandoffURI, auditID)
	res, err := tx.ExecContext(ctx, w.art,
		itemID, runID, HandoffKind, uri, digest, principal, fence)
	if err != nil {
		return HandoffWriteResult{}, fmt.Errorf("coord.ProdHandoffWriter.WriteHandoff: artifact: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Custody was lost between the two statements (a fenced reclaim
		// committed under us). Roll back — a handoff without its registered
		// artifact pointer, or vice versa, must never be observable.
		return HandoffWriteResult{}, ErrNotHandoffCustodian
	}

	if err := tx.Commit(); err != nil {
		return HandoffWriteResult{}, fmt.Errorf("coord.ProdHandoffWriter.WriteHandoff: commit: %w", err)
	}
	return HandoffWriteResult{AuditLogID: auditID, URI: uri, SHA256: digest}, nil
}

// AuditHandoffContent resolves AuditHandoffURI uris to the canonical HandoffDoc
// bytes: the audit row's payload, digest-verified against the registering
// artifact row's sha256 (fail-closed on tamper or pointer drift). The returned
// function is the v1 binding for ProdDispatcher's ArtifactContent hook (2.9);
// an 8.x object-store binding replaces it without touching the writer. db must
// be the same coordination store the writer and dispatcher are bound to.
func AuditHandoffContent(db *sql.DB) func(ctx context.Context, uri string) ([]byte, error) {
	if db == nil {
		panic("coord.AuditHandoffContent: nil db")
	}
	return func(ctx context.Context, uri string) ([]byte, error) {
		if !strings.HasPrefix(uri, AuditHandoffURI) {
			return nil, fmt.Errorf("coord.AuditHandoffContent: unsupported uri %q (v1 binds %s<audit_log id>)",
				uri, AuditHandoffURI)
		}
		id := strings.TrimPrefix(uri, AuditHandoffURI)

		// Join the registering artifact rows so the digest check reads a
		// digest OF RECORD, not a caller-supplied one: the bytes at a handoff
		// uri must hash to exactly what coord.artifact registered for them.
		// payload::text is the stored jsonb canonical form — byte-identical
		// to what WriteHandoff hashed at registration time. The schema's
		// uniqueness is (work_item, run, kind), NOT uri — several rows may
		// register the same uri, and a plain row-pick would verify against an
		// ARBITRARY one of them — so verification is against the SET of
		// registered digests: matching any of them proves the bytes are the
		// registered ones. Which exact row a caller is being served is then
		// enforced per-row by the reader (artifactbrowser.ProdStore.Content).
		rows, err := db.QueryContext(ctx, `
			SELECT al.payload::text, art.sha256
			  FROM coord.audit_log al
			  JOIN coord.artifact art ON art.uri = $1
			 WHERE al.id = $2::bigint`, uri, id)
		if err != nil {
			return nil, fmt.Errorf("coord.AuditHandoffContent: read %s: %w", uri, err)
		}
		defer rows.Close()
		var payload []byte
		registered := make(map[string]struct{})
		for rows.Next() {
			var p []byte
			var want string
			if err := rows.Scan(&p, &want); err != nil {
				return nil, fmt.Errorf("coord.AuditHandoffContent: read %s: %w", uri, err)
			}
			payload = p
			registered[want] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("coord.AuditHandoffContent: read %s: %w", uri, err)
		}
		if len(registered) == 0 || payload == nil {
			return nil, fmt.Errorf("coord.AuditHandoffContent: no handoff content at %s", uri)
		}
		got := sha256.Sum256(payload)
		if _, ok := registered[hex.EncodeToString(got[:])]; !ok {
			return nil, fmt.Errorf("coord.AuditHandoffContent: content digest mismatch at %s — "+
				"payload no longer hashes to the registered sha256 (tamper or pointer drift)", uri)
		}
		return payload, nil
	}
}
