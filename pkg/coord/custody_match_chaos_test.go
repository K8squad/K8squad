//go:build chaos

// custody_match_chaos_test.go — ADR-0024a S5 (ISI-4871): the identity
// round-trip that proves a dispatched agent's run capability token (S3,
// pkg/mcpauthtoken) and the dispatch claim it must match (coord.claim.
// assignee_agent, stamped at checkout) carry the SAME identity string, so the
// custody gate (workitemauthor.go agentHoldsClaim) PASSES for a correctly-
// granted agent instead of failing closed on a silent string mismatch.
//
// This is a VERIFICATION slice, not a new mechanism. The custody gate already
// exists and is covered by workitemauthor_chaos_test.go; those tests stamp
// assignee_agent with a literal that trivially equals the AgentName they pass.
// S5 closes the gap they leave open: it proves the two PRODUCTION derivations
// of the identity string actually agree, tying both to ONE api.Run:
//
//	Token side  (pkg/controller/run/assembly.go:339, ensureAuthoringToken):
//	    mcpauthtoken token `AgentID` claim = run.Spec.Agents[0].Name.
//	    → memory /mcp verifies the token, builds mcpSession.agentID from the
//	      VERIFIED claim, discards client X-* (S3) → internal/memory/
//	      agentauthor.go:255 folds it to id.agentName → AgentCreateWorkItem
//	      .AgentName.
//	Claim side  (pkg/controller/rundrive/driver.go:419 + firstAgentName:751,
//	    → store.go Acquire → coord ProdClaimer.AcquireSpecific):
//	    coord.claim.assignee_agent = firstAgentName(run) = run.Spec.Agents[0].Name.
//
// Both are the Agent CR name (ObjectRef.Name). If one side ever drifted to a
// display name, principal, or UID, custody would fail closed for a granted
// agent — a silent, confusing denial. The negative sub-test below gives that
// failure mode teeth: a claim stamped under a different identity string is
// refused ErrAgentAuthorNotInCustody even though the agent is otherwise valid.
//
// The claim is stamped through coord's REAL AcquireSpecific (the same call
// store.go makes at checkout), not a hand-UPDATE, so the DB read of
// assignee_agent is the production value. FR-B3: this file adds no exported
// pkg/coord symbol (external coord_test package), so the §68 allowlist gate is
// untouched.
//
// CI EXECUTION: the entry point is named TestSpineCustodyMatch so the spine
// chaos lane's `go test -race -tags=chaos -run 'TestSpine'` (spine-chaos.yml:241)
// actually runs it, matching the TestSpineProdDispatch / TestSpineProdClaim
// convention. It self-provisions the schema it needs — the base spine (0001),
// the assignee_agent column (0018) AND the create-field columns (0020) that
// AgentCreateWorkItem writes — via custodyResetSchema; it does NOT reuse
// resetAuthorSchema (which stops at 0018 and would 42703 on the priority insert;
// that gap is latent because the TestAgentCreate_* tests are not TestSpine-named
// and so never run in the chaos lane).
package coord_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// A real uuid: the Run's UID doubles as coord.claim.run_id and the token's
	// RunID, so all three ::uuid casts in the round-trip agree.
	custodyRunUID = "00000000-0000-0000-0000-0000000000c5"
	// >= 32 bytes: the HS256 test signing key. In production the same
	// KSQUAD_JWT_SIGNING_KEY reaches the control plane (mint) and cmd/memory
	// (verify) via Helm (ADR-0024a D2 option (a)).
	custodySigningKey = "0123456789abcdef0123456789abcdef"
	// The operator principal that performs the §6.2 checkout acquire.
	custodyOperatorPrincipal = "principal:operator"
)

// custodyResetSchema re-applies the base spine (0001) + assignee_agent (0018) +
// the create-field columns (0020: priority/work_mode/labels) into a clean coord
// schema, so the real AgentCreateWorkItem insert has every column it writes. The
// teeth bite the shipped DDL exactly as the apiserver migration runner
// provisions it.
func custodyResetSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, assigneeMigrationSQL(t))
	mustExec(t, db, createFieldsMigrationSQL(t))
}

// createFieldsMigrationSQL locates the shipped 0020 migration, mirroring
// coordMigrationSQL's candidate paths.
func createFieldsMigrationSQL(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "0020_work_item_create_fields.sql"))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "db", "migrations", "0020_work_item_create_fields.sql"),
		filepath.Join("db", "migrations", "0020_work_item_create_fields.sql"),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("cannot locate 0020_work_item_create_fields.sql (looked in %v)", candidates)
	return ""
}

// custodyDispatchRun builds the api.Run whose spec.Agents[0].Name is the SINGLE
// source of truth both sides of the round-trip derive their identity string
// from — exactly as assembly.go (token) and driver.go/store.go (claim) do.
func custodyDispatchRun() *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-quill", UID: types.UID(custodyRunUID)},
		Spec: api.RunSpec{
			TeamRef: api.ObjectRef{Name: "demo-team"},
			Agents:  []api.ObjectRef{{Name: "quill"}}, // the dispatched, decomposing agent (Agent CR name)
			OwnedBy: api.PrincipalRef("user:henrik"),
		},
	}
}

// mintRunTokenAgentID mirrors assembly.go:333 ensureAuthoringToken: mint the run
// capability token with AgentID = run.Spec.Agents[0].Name, then Verify it and
// return the agentID the memory /mcp edge would build mcpSession.agentID from.
// This is the token END of the round-trip — produced by the real minter, not a
// literal.
func mintRunTokenAgentID(t *testing.T, run *api.Run) string {
	t.Helper()
	minter, err := mcpauthtoken.NewMinter([]byte(custodySigningKey), 0)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	token, err := minter.Mint(mcpauthtoken.Claims{
		TeamID:    run.Spec.TeamRef.Name,
		Principal: string(run.GetOwnedBy()),
		AgentID:   run.Spec.Agents[0].Name, // == assembly.go:339
		RunID:     string(run.UID),
		// Baked from the S4 grant at mint (mirrors capability.CapabilityWorkItemAuthor);
		// the coord custody gate never reads caps — that gate lives at the memory edge.
		Capabilities: []string{"work_item.author"},
	})
	if err != nil {
		t.Fatalf("mint run capability token: %v", err)
	}
	verified, err := minter.Verify(token)
	if err != nil {
		t.Fatalf("verify run capability token: %v", err)
	}
	return verified.AgentID // → X-Agent-Id (verified, not client-supplied) → id.agentName
}

// seedClaimableItem inserts a claimable ('todo') work_item under the author
// tenancy; the F3 provision trigger gives it exactly one unheld claim row ready
// for AcquireSpecific.
func seedClaimableItem(t *testing.T, db *sql.DB, title string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, team_id, title, state, created_by)
		VALUES ($1::uuid, $2::uuid, $3, 'todo', 'user:henrik')
		RETURNING id::text`, authorProject, authorTeam, title).Scan(&id); err != nil {
		t.Fatalf("seed claimable work_item: %v", err)
	}
	return id
}

// stampAssigneeViaCheckout drives coord's REAL §6.2 acquire (the same call
// rundrive store.go makes) with assigneeAgent = run.Spec.Agents[0].Name, then
// returns the assignee_agent the DB actually stored — the claim END of the
// round-trip.
func stampAssigneeViaCheckout(t *testing.T, db *sql.DB, run *api.Run, itemID string) string {
	t.Helper()
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	// firstAgentName(run) == run.Spec.Agents[0].Name (driver.go:751).
	_, _, ok, err := pc.AcquireSpecific(context.Background(), custodyOperatorPrincipal, string(run.UID), itemID, "", run.Spec.Agents[0].Name)
	if err != nil {
		t.Fatalf("AcquireSpecific checkout: %v", err)
	}
	if !ok {
		t.Fatalf("AcquireSpecific: guard rejected the checkout of a fresh claimable item")
	}
	var assignee sql.NullString
	if err := db.QueryRow(`SELECT assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid`, itemID).Scan(&assignee); err != nil {
		t.Fatalf("read back assignee_agent: %v", err)
	}
	if !assignee.Valid {
		t.Fatalf("assignee_agent NULL after checkout with a non-empty agent name")
	}
	return assignee.String
}

// TestSpineCustodyMatch is the S5 chaos entry point. The TestSpine prefix is
// load-bearing: spine-chaos.yml runs `-run 'TestSpine'`, so this is the name that
// actually executes the custody round-trip in CI.
func TestSpineCustodyMatch(t *testing.T) {
	dsn := dsnOrFatal(t)
	t.Run("token_agentID_equals_claim_assignee_custody_passes", func(t *testing.T) {
		custodyMatchPasses(t, dsn)
	})
	t.Run("identity_string_mismatch_fails_closed", func(t *testing.T) {
		custodyMismatchFailsClosed(t, dsn)
	})
}

// custodyMatchPasses — the S5 acceptance: the token's agentID and the dispatch
// claim's assignee_agent are byte-equal (both run.Spec.Agents[0].Name), and
// AgentCreateWorkItem driven under the token's agentID passes the custody gate.
// Also covers the descendant custody edge (grandchild via the parent-walk) and
// the lease-expiry edge (the durable attribution match, not a live lease,
// carries custody).
func custodyMatchPasses(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	custodyResetSchema(t, db)
	run := custodyDispatchRun()

	// Both ENDS of the round-trip, each from its real production derivation.
	tokenAgentID := mintRunTokenAgentID(t, run)
	parent := seedClaimableItem(t, db, "epic")
	claimAssignee := stampAssigneeViaCheckout(t, db, run, parent)

	// AC #1: the two identity strings agree. If this ever fails, S5 AC #2 says
	// fix the plumbing so both canonicalize to the Agent CR name.
	if tokenAgentID != claimAssignee {
		t.Fatalf("identity mismatch: token agentID %q != claim assignee_agent %q "+
			"(custody would fail closed for a granted agent)", tokenAgentID, claimAssignee)
	}
	if tokenAgentID != run.Spec.Agents[0].Name {
		t.Fatalf("round-trip drifted from the Agent CR name: got %q, want %q", tokenAgentID, run.Spec.Agents[0].Name)
	}

	// AC #3 (custody match): AgentCreateWorkItem under the TOKEN's agentID passes
	// the gate — the attribution path (assignee_agent == agentName) in
	// agentHoldsClaim. Principal here is NOT the checkout holder, proving custody
	// rides the durable attribution, not the lease/holder.
	s := newAuthorStore(t, db)
	child, err := s.AgentCreateWorkItem(context.Background(), coord.AgentCreateWorkItemInput{
		ParentID:  parent,
		Title:     "story",
		Principal: string(run.GetOwnedBy()),
		AgentName: tokenAgentID,
		RunID:     string(run.UID),
	})
	if err != nil {
		t.Fatalf("custody-matched create: got %v, want success", err)
	}

	// Custody boundary for CREATE (ADR-0024 §4.1): creation requires custody of
	// the DIRECT parent (agentHoldsClaim), NOT the item-and-descendants scope
	// UPDATE uses (agentHoldsCustodyScope, O-3). The freshly-created child carries
	// no assignee_agent, so a grandchild under it is refused — nesting past one
	// level requires the intermediate level to actually be in custody. This is the
	// deny-by-default reading; widening CREATE to the scope walk would be an
	// ADR-0024a decision (flagged in the S5 report, not decided by a verify story).
	if _, err := s.AgentCreateWorkItem(context.Background(), coord.AgentCreateWorkItemInput{
		ParentID:  child.ID,
		Title:     "task",
		Principal: string(run.GetOwnedBy()),
		AgentName: tokenAgentID,
		RunID:     string(run.UID),
	}); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("grandchild under unheld child: got %v, want ErrAgentAuthorNotInCustody (create needs direct-parent custody)", err)
	}

	// Once the intermediate child is itself in the agent's custody under the SAME
	// identity string (exactly what a real dispatch of that child stamps into its
	// assignee_agent — the same round-trip, one level down), a grandchild under it
	// IS created. Nested decomposition works when the identity string matches at
	// each level — the whole point of the round-trip holding end to end.
	mustExec(t, db, `UPDATE coord.claim SET assignee_agent = $1 WHERE work_item_id = $2::uuid`, tokenAgentID, child.ID)
	if _, err := s.AgentCreateWorkItem(context.Background(), coord.AgentCreateWorkItemInput{
		ParentID:  child.ID,
		Title:     "task",
		Principal: string(run.GetOwnedBy()),
		AgentName: tokenAgentID,
		RunID:     string(run.UID),
	}); err != nil {
		t.Fatalf("grandchild under custody-held child: got %v, want success", err)
	}

	// Lease-expiry edge: expire the parent's lease and confirm custody survives —
	// agentHoldsClaim accepts the durable assignee_agent match even with a dead
	// lease, so a long decomposition run never loses custody mid-flight.
	mustExec(t, db, `UPDATE coord.claim SET lease_expires_at = now() - interval '1 hour' WHERE work_item_id = $1::uuid`, parent)
	if _, err := s.AgentCreateWorkItem(context.Background(), coord.AgentCreateWorkItemInput{
		ParentID:  parent,
		Title:     "story-after-lease-expiry",
		Principal: string(run.GetOwnedBy()),
		AgentName: tokenAgentID,
		RunID:     string(run.UID),
	}); err != nil {
		t.Fatalf("post-lease-expiry create: got %v, want success (durable attribution)", err)
	}
}

// custodyMismatchFailsClosed — the teeth: if the claim were stamped under a
// DIFFERENT identity string than the token carries (a principal or UID instead
// of the Agent CR name), custody fails closed for an otherwise-valid, granted
// agent. This is the exact silent-failure S5 exists to prevent — the positive
// test only means something because this negative one does too.
func custodyMismatchFailsClosed(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	custodyResetSchema(t, db)
	run := custodyDispatchRun()
	tokenAgentID := mintRunTokenAgentID(t, run) // "quill" (Agent CR name)

	parent := seedClaimableItem(t, db, "epic")
	// Stamp the claim under a principal-shaped string, the classic drift the
	// story warns about ("agent:quill" != "quill").
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	wrongIdentity := "agent:" + run.Spec.Agents[0].Name
	if wrongIdentity == tokenAgentID {
		t.Fatalf("test setup: wrong identity %q must differ from token agentID %q", wrongIdentity, tokenAgentID)
	}
	if _, _, ok, err := pc.AcquireSpecific(context.Background(), custodyOperatorPrincipal, string(run.UID), parent, "", wrongIdentity); err != nil || !ok {
		t.Fatalf("AcquireSpecific with mismatched identity: ok=%v err=%v", ok, err)
	}

	// A granted agent whose token agentID does not equal the stamped
	// assignee_agent is refused — existence-hiding ErrAgentAuthorNotInCustody.
	s := newAuthorStore(t, db)
	if _, err := s.AgentCreateWorkItem(context.Background(), coord.AgentCreateWorkItemInput{
		ParentID:  parent,
		Title:     "story",
		Principal: string(run.GetOwnedBy()),
		AgentName: tokenAgentID,
		RunID:     string(run.UID),
	}); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("identity mismatch: got %v, want ErrAgentAuthorNotInCustody (custody must fail closed)", err)
	}
	assertChildCount(t, db, parent, 0)
}
