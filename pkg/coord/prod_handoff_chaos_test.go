//go:build chaos

// ISI-2525 — TestSpineProdHandoff (H1..H4): the Story 2.8 structured handoff
// artifact writer (prodhandoff.go) proved against the SHIPPED coord schema
// (db/migrations/0001_coord_schema.sql) on a real Postgres, inside the same
// required gate as TestSpine/TestSpineProdClaim (the workflow's -run
// 'TestSpine' matches this name too).
//
//		H1  custody-gated write — only the live fence-matching holder registers a
//		    handoff; the bytes at the artifact uri roundtrip to the doc and hash
//		    to the registered sha256. Teeth: an UNGUARDED writer MUST land the
//		    same zombie's rows, proving the refusal comes from the guard, not the
//		    schema.
//		H2  ADVISORY-ONLY — a handoff mutates neither coord.claim (holder, fence,
//		    lease) nor coord.work_item (state): custody stays fenced
//		    release→re-dispatch→claim (§8.5). Teeth: a naive handoff-with-release
//		    arm MUST mutate both, proving the snapshot comparison can fail.
//	  H3  idempotent re-entry (§6.4) — re-drives republish in place, never
//	      duplicate; concurrent same-run writers converge on one artifact row.
//	      Teeth: the shipped UNIQUE key must reject a plain duplicate (23505),
//	      and the republish must UPDATE content — a DO-NOTHING upsert would
//	      leave the resolver serving stale findings and fail (C).
//		H4  lease expiry fences the write — a lapsed former holder is refused
//		    even at its own (once-valid) fence, and the NEXT claimant's handoff
//		    lands as a separate run-keyed artifact. Teeth: the unguarded writer
//		    sails through post-expiry, proving the guard is the lease term.
//
// Arms that need a clean record between the teeth phase and the guarded phase
// re-apply the whole migration (resetProdSchema) rather than deleting rows —
// coord.artifact/audit_log are append-only BY TRIGGER, which is itself part
// of the schema under test.
package coord_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// handoffLease is the lease H4 installs on its claim row: long enough to
// write while live, short enough that a real sleep crosses expiry.
const handoffLease = "1 second"

// zombieRunUUID keys the unguarded control arm's rows so the teeth never
// collide with the shipped UNIQUE (work_item_id, run_id, kind). Valid uuids,
// disjoint from prodRunUUID's low counter range.
func zombieRunUUID(tag int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", 0xbeef0000+tag)
}

// installHolder deterministically installs (principal, run, fence) on the
// offset-th seeded item's PRE-PROVISIONED claim row with a lease of the given
// interval, and advances the item to in_progress — the state a §6.2 claim
// would have left it in. Returns the item id. (Direct SQL, not ProdClaimer:
// the unit under test is the handoff writer; the claim path is P1's.)
func installHolder(t *testing.T, db *sql.DB, offset int, principal, run string, fence int64, lease string) string {
	t.Helper()
	var item string
	if err := db.QueryRow(`SELECT id::text FROM coord.work_item ORDER BY created_at, id OFFSET $1 LIMIT 1`, offset).Scan(&item); err != nil {
		t.Fatalf("pick seed item %d: %v", offset, err)
	}
	mustExec(t, db, `
		UPDATE coord.claim
		   SET holder_principal = $1, run_id = $2::uuid, fence_token = $3,
		       lease_expires_at = clock_timestamp() + interval '`+lease+`',
		       acquired_at = clock_timestamp()
		 WHERE work_item_id = $4::uuid`, principal, run, fence, item)
	mustExec(t, db, `UPDATE coord.work_item SET state='in_progress' WHERE id=$1::uuid`, item)
	return item
}

// claimSnapshot is the custody state H2 pins byte-for-byte across a handoff.
type claimSnapshot struct {
	holder sql.NullString
	run    sql.NullString
	fence  int64
	lease  sql.NullTime
}

func snapshotClaim(t *testing.T, db *sql.DB, item string) claimSnapshot {
	t.Helper()
	var s claimSnapshot
	if err := db.QueryRow(`
		SELECT holder_principal, run_id, fence_token, lease_expires_at
		  FROM coord.claim WHERE work_item_id = $1::uuid`, item).
		Scan(&s.holder, &s.run, &s.fence, &s.lease); err != nil {
		t.Fatalf("snapshot claim: %v", err)
	}
	return s
}

func itemState(t *testing.T, db *sql.DB, item string) string {
	t.Helper()
	var st string
	if err := db.QueryRow(`SELECT state FROM coord.work_item WHERE id=$1::uuid`, item).Scan(&st); err != nil {
		t.Fatalf("read item state: %v", err)
	}
	return st
}

// countRows is the teeth-friendly existence check the arms lean on.
func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// sampleHandoffDoc is a populated seven-field doc (every Story 2.8 field set,
// so a serialization that dropped any field fails the roundtrip).
func sampleHandoffDoc() coord.HandoffDoc {
	return coord.HandoffDoc{
		Did:       []string{"implemented pkg/coord writer", "added chaos arms H1..H4"},
		Decisions: []string{"content lives in the audit payload; artifact uri points at it"},
		Next:      []string{"wire the apiserver run loop to call WriteHandoff before Complete"},
		Blockers:  []string{"none"},
		Findings:  "the shipped UNIQUE (work_item_id, run_id, kind) makes re-entry a republish",
		RecommendedNext: []coord.DraftWorkItem{
			{Title: "Bind ArtifactContent in the apiserver", Body: "swap AuditHandoffContent for the object store when 8.x lands"},
		},
		ArtifactsForDownstream: []coord.ArtifactRef{
			{Kind: "handoff", URI: "coord+audit://0", SHA256: "placeholder"},
		},
	}
}

// naiveWriteHandoff is the UNGUARDED control arm: the same two writes
// (audit append + artifact insert) with NO custody predicate anywhere — what
// the schema alone permits. The differential teeth use it to prove that when
// the real writer refuses a zombie, refusal is the GUARD's doing.
func naiveWriteHandoff(t *testing.T, db *sql.DB, item, principal, run string, fence int64, doc coord.HandoffDoc) {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("naive marshal: %v", err)
	}
	sum := sha256.Sum256(body)
	mustExec(t, db, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, payload)
		VALUES ($1::uuid, $2::uuid, 'artifact_registered', $3, $4, $5::jsonb)`,
		item, run, principal, fence, body)
	mustExec(t, db, `
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
		VALUES ($1::uuid, $2::uuid, 'handoff', $3, $4)`,
		item, run, "coord+audit://naive", hex.EncodeToString(sum[:]))
}

func TestSpineProdHandoff(t *testing.T) {
	dsn := dsnOrFatal(t)
	t.Run("H1_custody_gated_write_roundtrips", func(t *testing.T) { prodH1CustodyGate(t, dsn) })
	t.Run("H2_advisory_only_no_custody_mutation", func(t *testing.T) { prodH2AdvisoryOnly(t, dsn) })
	t.Run("H3_idempotent_reentry_republish", func(t *testing.T) { prodH3IdempotentReentry(t, dsn) })
	t.Run("H4_lease_expiry_fences_write", func(t *testing.T) { prodH4LeaseExpiry(t, dsn) })
}

// --------------------------- H1 ---------------------------------------------
// The live holder's write lands exactly once, content-addressed and
// provenance-tagged; nobody else's does — wrong principal, stale fence,
// foreign run are all refused with NOTHING written. Teeth: the naive
// unguarded writer lands the zombie's rows under identical conditions, so the
// refusals are the guard's work, not schema luck.
func prodH1CustodyGate(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()

	// (A) teeth — the unguarded writer MUST land a zombie's rows.
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
	item := installHolder(t, db, 0, "agent-a", prodRunUUID(1), 1, "30 seconds")
	naiveWriteHandoff(t, db, item, "zombie", zombieRunUUID(1), 99, sampleHandoffDoc())
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff'`); n != 1 {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: naive unguarded write landed %d handoff artifacts, "+
			"want 1 — the schema permits unguarded writes, so H1's refusals below must come from the guard", n)
	}

	// Clean record for the guarded phase (append-only tables: reset the schema,
	// never delete rows).
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
	item = installHolder(t, db, 0, "agent-a", prodRunUUID(1), 1, "30 seconds")

	w, err := coord.NewProdHandoffWriter(db)
	if err != nil {
		t.Fatalf("NewProdHandoffWriter: %v", err)
	}
	doc := sampleHandoffDoc()

	// (B) the live holder writes — once, content-addressed, provenanced.
	res, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc)
	if err != nil {
		t.Fatalf("live holder WriteHandoff: %v", err)
	}
	if n := countRows(t, db,
		`SELECT count(*) FROM coord.artifact
		  WHERE work_item_id=$1::uuid AND run_id=$2::uuid AND kind='handoff'`,
		item, prodRunUUID(1)); n != 1 {
		t.Fatalf("want exactly 1 handoff artifact for (item, run), got %d", n)
	}
	if n := countRows(t, db,
		`SELECT count(*) FROM coord.audit_log
		  WHERE event_type='artifact_registered' AND principal='agent-a'
		    AND run_id=$2::uuid AND fence_token=1 AND work_item_id=$1::uuid`,
		item, prodRunUUID(1)); n != 1 {
		t.Fatalf("want the §6.5 audit row (principal+run+fence provenanced) appended, got %d", n)
	}

	// (C) the uri resolves to bytes that ARE the doc, digest-verified: the
	// digest must match the BYTES AT REST (jsonb-normalized), not any
	// Go-side re-marshal — that equivalence is the content-addressing contract.
	resolve := coord.AuditHandoffContent(db)
	raw, err := resolve(ctx, res.URI)
	if err != nil {
		t.Fatalf("resolve %s: %v", res.URI, err)
	}
	gotDigest := sha256.Sum256(raw)
	if hex.EncodeToString(gotDigest[:]) != res.SHA256 {
		t.Fatalf("resolved bytes hash %s != registered sha256 %s — content addressing is broken",
			hex.EncodeToString(gotDigest[:]), res.SHA256)
	}
	var back coord.HandoffDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("resolved bytes are not a HandoffDoc: %v", err)
	}
	if mustJSON(t, back) != mustJSON(t, doc) {
		t.Fatalf("handoff roundtrip mismatch:\n got %s\nwant %s", mustJSON(t, back), mustJSON(t, doc))
	}
	if _, err := resolve(ctx, "coord+audit://999999999"); err == nil {
		t.Fatalf("resolver must fail-closed on a dangling uri, got success")
	}

	// (D) refusals — each zombie variant writes NOTHING.
	refusals := []struct {
		name      string
		principal string
		run       string
		fence     int64
	}{
		{"wrong_principal", "zombie", prodRunUUID(1), 1},
		{"stale_fence", "agent-a", prodRunUUID(1), 0},
		{"foreign_run", "agent-a", zombieRunUUID(1), 1},
	}
	arts := countRows(t, db, `SELECT count(*) FROM coord.artifact`)
	auds := countRows(t, db, `SELECT count(*) FROM coord.audit_log`)
	for _, z := range refusals {
		_, err := w.WriteHandoff(ctx, item, z.principal, z.run, "", z.fence, doc)
		if !errors.Is(err, coord.ErrNotHandoffCustodian) {
			t.Fatalf("%s: want ErrNotHandoffCustodian, got %v", z.name, err)
		}
		if n := countRows(t, db, `SELECT count(*) FROM coord.artifact`); n != arts {
			t.Fatalf("%s: refused write still changed coord.artifact (%d -> %d)", z.name, arts, n)
		}
		if n := countRows(t, db, `SELECT count(*) FROM coord.audit_log`); n != auds {
			t.Fatalf("%s: refused write still changed coord.audit_log (%d -> %d)", z.name, auds, n)
		}
	}
}

// --------------------------- H2 ---------------------------------------------
// Advisory ONLY: across the live holder's write, the claim row (holder, run,
// fence, lease term) and the item's lane are byte-identical — the handoff
// informs, it never moves custody. Teeth: the naive handoff-with-release arm
// DOES bump the fence, clear the holder and advance the lane, proving the
// snapshot comparison detects exactly the mutation class the story forbids.
func prodH2AdvisoryOnly(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()
	resetProdSchema(t, db)
	seedProdItems(t, db, 2)
	item := installHolder(t, db, 0, "agent-a", prodRunUUID(1), 1, "30 seconds")
	teethItem := installHolder(t, db, 1, "agent-teeth", prodRunUUID(2), 5, "30 seconds")

	// (A) teeth — naive "handoff that also releases custody" mutates all three.
	tBefore := snapshotClaim(t, db, teethItem)
	mustExec(t, db, `
		UPDATE coord.claim
		   SET holder_principal = NULL, run_id = NULL, fence_token = fence_token + 1,
		       lease_expires_at = NULL
		 WHERE work_item_id = $1::uuid`, teethItem)
	mustExec(t, db, `UPDATE coord.work_item SET state='done' WHERE id=$1::uuid`, teethItem)
	tAfter := snapshotClaim(t, db, teethItem)
	if tAfter == tBefore || itemState(t, db, teethItem) != "done" {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: the naive release arm did not observably mutate " +
			"custody (claim/lease/state), so H2's no-mutation pass proves nothing")
	}

	// (B) the real handoff — custody byte-identical, lane unchanged.
	w, _ := coord.NewProdHandoffWriter(db)
	before := snapshotClaim(t, db, item)
	stateBefore := itemState(t, db, item)
	if _, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, sampleHandoffDoc()); err != nil {
		t.Fatalf("WriteHandoff: %v", err)
	}
	after := snapshotClaim(t, db, item)
	if after != before {
		t.Fatalf("ADVISORY-ONLY VIOLATED: handoff mutated the claim row\n before %+v\n after  %+v "+
			"(custody moves only via fenced release→re-dispatch→claim, §8.5)", before, after)
	}
	if st := itemState(t, db, item); st != stateBefore {
		t.Fatalf("ADVISORY-ONLY VIOLATED: handoff moved the item lane %q -> %q", stateBefore, st)
	}
}

// --------------------------- H3 ---------------------------------------------
// §6.4 re-entry: same (item, run) re-drives republish IN PLACE — one artifact
// row always; same content is a hash no-op, changed content updates the
// pointer; audit rows append per write (history). Concurrent same-run writers
// converge on one row. Teeth: a plain second INSERT duplicates, proving the
// count assertions can fail.
func prodH3IdempotentReentry(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()

	// (A) teeth — the SHIPPED UNIQUE (work_item_id, run_id, kind) itself
	// rejects a duplicate registration: the second plain INSERT must FAIL
	// loud (SQLSTATE 23505). If it landed, "one artifact row per (item, run)"
	// would be unenforced and the count assertions below would be vacuous.
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
	item := installHolder(t, db, 0, "agent-teeth", prodRunUUID(3), 1, "30 seconds")
	naiveWriteHandoff(t, db, item, "agent-teeth", prodRunUUID(3), 1, sampleHandoffDoc())
	if _, err := db.Exec(`
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
		VALUES ($1::uuid, $2::uuid, 'handoff', 'coord+audit://dup', 'x')`,
		item, prodRunUUID(3)); err == nil {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: a plain duplicate artifact registration LANDED — " +
			"the shipped upsert key does not bite, so one-row assertions below prove nothing")
	} else if !strings.Contains(err.Error(), "23505") {
		t.Fatalf("duplicate registration failed for the wrong reason (want unique violation 23505): %v", err)
	}

	// Clean record for the guarded phase.
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
	item = installHolder(t, db, 0, "agent-a", prodRunUUID(1), 1, "30 seconds")

	w, _ := coord.NewProdHandoffWriter(db)

	// (B) same-content re-drive: still one row, same digest.
	doc := sampleHandoffDoc()
	r1, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	r2, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc)
	if err != nil {
		t.Fatalf("re-drive write: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff'`); n != 1 {
		t.Fatalf("same-content re-drive: want 1 artifact row (republish no-op), got %d", n)
	}
	if r1.SHA256 != r2.SHA256 {
		t.Fatalf("same doc re-published under a different digest: %s -> %s", r1.SHA256, r2.SHA256)
	}

	// (C) changed-content re-drive: still one row, pointer + digest updated,
	// resolver yields the NEW doc (republish, never duplicate).
	doc.Findings = "updated after review feedback — blockers cleared"
	r3, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc)
	if err != nil {
		t.Fatalf("changed-content write: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff'`); n != 1 {
		t.Fatalf("changed-content re-drive: want 1 artifact row (in-place republish), got %d", n)
	}
	var uri, sha string
	if err := db.QueryRow(`SELECT uri, sha256 FROM coord.artifact WHERE kind='handoff'`).Scan(&uri, &sha); err != nil {
		t.Fatalf("read artifact row: %v", err)
	}
	if uri != r3.URI || sha != r3.SHA256 {
		t.Fatalf("artifact row (%s,%s) does not match the republish result (%s,%s)", uri, sha, r3.URI, r3.SHA256)
	}
	resolve := coord.AuditHandoffContent(db)
	raw, err := resolve(ctx, uri)
	if err != nil {
		t.Fatalf("resolve republished uri: %v", err)
	}
	var back coord.HandoffDoc
	if err := json.Unmarshal(raw, &back); err != nil || back.Findings != doc.Findings {
		t.Fatalf("resolver must yield the NEW doc after republish (findings=%q err=%v)", back.Findings, err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.audit_log WHERE event_type='artifact_registered'`); n != 3 {
		t.Fatalf("audit rows must append per write (3 writes), got %d", n)
	}

	// (D) concurrent same-run writers: still exactly one artifact row.
	const concurrent = 8
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := sampleHandoffDoc()
			d.Findings = fmt.Sprintf("concurrent re-drive %d", i)
			if _, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, d); err != nil {
				t.Errorf("concurrent write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff'`); n != 1 {
		t.Fatalf("concurrent same-run writers: want 1 converged artifact row, got %d", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.audit_log WHERE event_type='artifact_registered'`); n != 3+concurrent {
		t.Fatalf("want %d audit rows after %d more concurrent writes, got %d", 3+concurrent, concurrent, n)
	}
}

// --------------------------- H4 ---------------------------------------------
// The lease term is the fence on the advisory write: once it lapses, the
// FORMER holder is refused at its own once-valid fence, and the next
// claimant's handoff lands as its own run-keyed artifact alongside (not over)
// the old one. Teeth: the unguarded writer sails through post-expiry.
func prodH4LeaseExpiry(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)

	w, _ := coord.NewProdHandoffWriter(db)
	doc := sampleHandoffDoc()

	// (A) write while live.
	item := installHolder(t, db, 0, "agent-a", prodRunUUID(1), 1, handoffLease)
	if _, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc); err != nil {
		t.Fatalf("live write under short lease: %v", err)
	}

	// (B) let the lease lapse in real wall-clock time.
	time.Sleep(1200 * time.Millisecond)

	// (C) teeth — the unguarded writer sails through post-expiry (a different
	// run, so the artifact UNIQUE key does not collide with (A)'s row).
	naiveWriteHandoff(t, db, item, "agent-a", zombieRunUUID(4), 1, doc)
	if n := countRows(t, db, `SELECT count(*) FROM coord.audit_log WHERE run_id=$1::uuid`, zombieRunUUID(4)); n != 1 {
		t.Fatalf("FALSIFICATION LOST ITS TEETH: naive post-expiry write did not land (audit rows=%d, want 1) "+
			"— the schema has no lease term of its own, so H4's refusal below is the guard's", n)
	}
	arts := countRows(t, db, `SELECT count(*) FROM coord.artifact`) // (A) + teeth = 2
	auds := countRows(t, db, `SELECT count(*) FROM coord.audit_log`)

	// (D) the lapsed holder is refused at its own fence; nothing lands.
	if _, err := w.WriteHandoff(ctx, item, "agent-a", prodRunUUID(1), "", 1, doc); !errors.Is(err, coord.ErrNotHandoffCustodian) {
		t.Fatalf("lapsed holder: want ErrNotHandoffCustodian, got %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact`); n != arts {
		t.Fatalf("lapsed holder's refused write still changed coord.artifact (%d -> %d)", arts, n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.audit_log`); n != auds {
		t.Fatalf("lapsed holder's refused write still changed coord.audit_log (%d -> %d)", auds, n)
	}

	// (E) the NEXT claimant (fence+1, its own run, fresh lease) writes its own
	// handoff; both runs' artifacts coexist, keyed by run.
	mustExec(t, db, `
		UPDATE coord.claim
		   SET holder_principal='agent-b', run_id=$1::uuid, fence_token=fence_token+1,
		       lease_expires_at = clock_timestamp() + interval '30 seconds'
		 WHERE work_item_id=$2::uuid`, prodRunUUID(2), item)
	res, err := w.WriteHandoff(ctx, item, "agent-b", prodRunUUID(2), "", 2, doc)
	if err != nil {
		t.Fatalf("next claimant write: %v", err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff' AND run_id=$1::uuid`, prodRunUUID(2)); n != 1 {
		t.Fatalf("next claimant: want exactly its own run-keyed handoff row, got %d", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM coord.artifact WHERE kind='handoff' AND run_id=$1::uuid`, prodRunUUID(1)); n != 1 {
		t.Fatalf("the lapsed run's legitimate earlier handoff must remain intact (run-keyed coexistence), got %d", n)
	}
	if _, err := coord.AuditHandoffContent(db)(ctx, res.URI); err != nil {
		t.Fatalf("next claimant's handoff must resolve: %v", err)
	}
}

// mustJSON marshals deterministically for comparisons (a marshal failure is a
// test bug, not a SUT failure).
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
