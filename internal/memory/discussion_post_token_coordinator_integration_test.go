package memory

// ISI-5694 — reproduction-first integration coverage for the party-round stall (ISI-5662 WS3).
//
// THE BAR (board, 2026-10-12). After four sequential WS3 live-smoke defects that each passed unit tests but
// failed the live party smoke, the board raised the acceptance bar: do NOT close on a unit mock that injects
// sess.capabilities / authorIsCoordinator. Drive a REAL discussion_post MCP call whose coordinator signal is
// DERIVED by the actual credential path — pkg/mcpauthtoken Mint → Verify → the /mcp handle's token-auth
// session build → SessionHasCoordinatorCapability → callDiscussionPost's authorIsCoordinator → the party
// dispatch hook — and show where the "no coordinator — fan-out suppressed" symptom does, and does not,
// reproduce.
//
// These two cases exercise that whole server edge over httptest with a VERIFIED bearer token and NO X-*
// identity headers (the sandbox transport). The only fakes are the leaf seams that are NOT under test (the
// discussion store, the party-session store, the roster, the dispatcher) — the coordinator capability is
// never injected; it rides a real signed token end to end.
//
// FINDING. The server edge propagates the coordinator marker CORRECTLY: a token minted with the coordinator
// capability drives authorIsCoordinator=true and the party roster is stamped (TestCoordinatorMarker…), while
// a token minted WITHOUT it reproduces the exact live suppression (TestNonCoordinatorToken…). So the live
// defect — a facilitator whose discussion-token Secret decodes to scp=[discussion,coordinator] yet still hits
// "no coordinator" — cannot originate at or below this edge: by construction, a coordinator-scoped token
// verified here yields the marker. The surviving gap is therefore upstream of Verify — which credential the
// dispatched opencode runtime actually presents when it calls discussion_post — and that is the one layer
// this in-process test cannot cover (it has no opencode client). See the ISI-5694 thread for the escalation.

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
)

// mcpTestSigningKey is a ≥32-byte HS256 key — the SAME shared-key model the control plane mints with and
// cmd/memory verifies under (ADR-0024a D2 option (a)). One minter both mints and verifies here, exactly as
// a shared KSQUAD_JWT_SIGNING_KEY lets the operator mint and memory verify.
var mcpTestSigningKey = []byte("isi5694-integration-test-signing-key-32+")

// mintRunToken mints a run discussion token with the given capabilities, mirroring assembly.go's
// ensureDiscussionToken (which bakes caps=[discussion] plus capability.CapabilityCoordinator when the run's
// agent holds the Team Coordinator role). Fails the test on a mint error.
func mintRunToken(t *testing.T, m *mcpauthtoken.Minter, teamID uuid.UUID, agent string, caps []string) string {
	t.Helper()
	tok, err := m.Mint(mcpauthtoken.Claims{
		TeamID:       teamID.String(),
		Principal:    "ksquad-intake",
		AgentID:      agent,
		RunID:        uuid.NewString(),
		Capabilities: caps,
	})
	if err != nil {
		t.Fatalf("mint run token: %v", err)
	}
	return tok
}

// partyMCPServer builds the discussion_post MCP edge with the token-auth path ENABLED (a real verifier over
// the shared key) and the party-dispatch bundle wired to the supplied fake party store. It returns the mux
// the test drives and the fake writer so the test can assert the post actually reached the fenced store.
func partyMCPServer(t *testing.T, minter *mcpauthtoken.Minter, store *fakeMemPartyStore) (*http.ServeMux, *fakeDiscussionWriter) {
	t.Helper()
	dw := &fakeDiscussionWriter{}
	dd := NewDiscussionDispatch(&captureDispatcher{}, fixedHopResolver{}, staticRoster{names: []string{"sam", "uma", "john"}}, nil).
		WithPartyStore(store)
	mux := http.NewServeMux()
	NewToolMCP(NewReadService(&fakeSearcher{}, NewHashingEmbedder()), nil, dw).
		WithDiscussionDispatch(dd).
		WithAuthoringTokenAuth(NewMCPAuthTokenVerifier(minter)).
		Mount(mux)
	return mux, dw
}

// partyThreadID is the party thread the facilitator replies into; the fake party store returns an active
// session for it, so a coordinator post stamps the round roster.
const partyThreadID = "44444444-4444-4444-4444-444444444444"

// discussionPostRPC is the JSON-RPC tools/call envelope for a facilitator round directive reply: a single
// @-mention post naming two voices in speaking order, into the live party thread.
const discussionPostRPC = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"discussion_post","arguments":{` +
	`"project_id":"bmad-squad/bmad-demo-project","thread_id":"` + partyThreadID + `",` +
	`"body":"Round 1 — one voice at a time: @uma then @john"}}}`

// TestMCPDiscussionPostCoordinatorMarkerPropagatesFromVerifiedToken drives the discussion_post edge with a
// VERIFIED bearer token minted WITH the coordinator capability and asserts the party roster is stamped —
// proving the coordinator marker propagates correctly through Mint → Verify → session → authorIsCoordinator
// → the party hook, with NO injected flag and NO X-* headers.
func TestMCPDiscussionPostCoordinatorMarkerPropagatesFromVerifiedToken(t *testing.T) {
	minter, err := mcpauthtoken.NewMinter(mcpTestSigningKey, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	teamID := uuid.New()
	store := &fakeMemPartyStore{sess: activeMemSession()}
	mux, dw := partyMCPServer(t, minter, store)

	tok := mintRunToken(t, minter, teamID, "sam", []string{"discussion", CoordinatorCapability})
	resp := rpcCall(t, mux, map[string]string{"Authorization": "Bearer " + tok}, discussionPostRPC)

	if resp.Error != nil {
		t.Fatalf("discussion_post rpc error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("discussion_post returned a tool error for a verified coordinator token")
	}
	// The post reached the fenced store under the token-derived identity (never an X-* header).
	if !dw.postCalled {
		t.Fatalf("PostMessage was not called — the edge did not reach the store")
	}
	if dw.postAuth.AgentID == nil || *dw.postAuth.AgentID != "sam" {
		t.Fatalf("post author agent = %v, want token-derived \"sam\"", dw.postAuth.AgentID)
	}
	// The coordinator marker rode the token to the party hook: the round roster was stamped in @-mention
	// order. This is the whole point — authorIsCoordinator was DERIVED, not injected.
	if store.setCalled != 1 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times, want 1 — the verified coordinator token must stamp the party round roster", store.setCalled)
	}
	if len(store.setVoices) != 2 || store.setVoices[0] != "uma" || store.setVoices[1] != "john" {
		t.Fatalf("stamped voices %v, want [uma john]", store.setVoices)
	}
}

// TestMCPDiscussionPostNonCoordinatorTokenReproducesSuppression drives the SAME edge with a VERIFIED token
// minted WITHOUT the coordinator capability and asserts the party roster is NOT stamped — the in-process
// reproduction of the live "multi-mention with no coordinator — fan-out suppressed" symptom, driven by a
// real non-coordinator token. Together with the case above this pins the symptom to the capability being
// ABSENT from the presented token at the edge, not to a server-side propagation bug.
func TestMCPDiscussionPostNonCoordinatorTokenReproducesSuppression(t *testing.T) {
	minter, err := mcpauthtoken.NewMinter(mcpTestSigningKey, time.Hour)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	teamID := uuid.New()
	store := &fakeMemPartyStore{sess: activeMemSession()}
	mux, dw := partyMCPServer(t, minter, store)

	// A discussion token WITHOUT the coordinator marker — what the edge sees if opencode presents a
	// non-coordinator credential (e.g. the search/authoring endpoint's token) for the discussion_post call.
	tok := mintRunToken(t, minter, teamID, "sam", []string{"discussion"})
	resp := rpcCall(t, mux, map[string]string{"Authorization": "Bearer " + tok}, discussionPostRPC)

	if resp.Error != nil {
		t.Fatalf("discussion_post rpc error: %+v", resp.Error)
	}
	if _, isErr := resultContentText(t, resp.Result); isErr {
		t.Fatalf("discussion_post returned a tool error for a verified non-coordinator token")
	}
	// The message is durable (the post always commits) ...
	if !dw.postCalled {
		t.Fatalf("PostMessage was not called — the edge did not reach the store")
	}
	// ... but with no coordinator marker on the token the party hook is inert: the round roster is NOT
	// stamped, so the advancer has no roster to sequence and the round stalls at zero voices. This is the
	// exact live symptom, reproduced in-process from a REAL token rather than an injected flag.
	if store.setCalled != 0 {
		t.Fatalf("SetRoundFacilitatorMessage called %d times without the coordinator marker, want 0 (fan-out must be suppressed)", store.setCalled)
	}
}
