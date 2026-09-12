package apiserver

// workitemstate_contract_test.go — the cross-tree request-body contract for
// PATCH /api/work-items/{id}/state (ISI-4225).
//
// The 8.14a-era console client serialized {to, expectedFrom} while this
// handler decodes {toState, fromState}; the BFF forwards the body verbatim,
// so every browser drag-and-drop / quick-move 400'd with "toState required"
// (found in the M1.6 smoke, ISI-4132 demo @ 9952db54).
//
// This test closes that drift path in both directions with ONE shared source
// of truth: the exact JSON fixture the vitest side
// (console/test/tickets/api.test.ts) pins the browser client's serialized body
// against, byte-for-byte, is replayed here through the REAL handler. If either
// tree changes its spelling or key set, one of the two tests fails:
//
//   - console changes the client  ⇒ the vitest exact-string equality fails;
//   - apiserver changes the body  ⇒ this replay fails (or the key-set guard
//     trips when the fixture itself is edited to match only one side).
//
// The fixture lives in the console tree because it IS the console's request
// body; this test reaching across trees is the point (contract tests must bind
// both producers and consumers to the same bytes).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/pkg/coord"
)

// consoleStateTransitionFixture is the single source of truth shared with
// console/test/tickets/api.test.ts (relative to this package's dir).
const consoleStateTransitionFixture = "../../console/test/tickets/fixtures/state-transition-request.json"

// TestWorkItemStateConsoleBodyContract — the console client's exact serialized
// body (the shared fixture) must be accepted by the real handler, and the store
// must receive exactly the lane values the fixture declares.
func TestWorkItemStateConsoleBodyContract(t *testing.T) {
	raw, err := os.ReadFile(consoleStateTransitionFixture)
	if err != nil {
		t.Fatalf("console contract fixture unreadable — the cross-tree state-transition contract is broken: %v", err)
	}

	// Guard the fixture itself: exactly the two contract keys, nothing else.
	// The stale 8.14a spelling {to, expectedFrom} must never creep back in
	// (the handler silently ignores unknown keys, so only a key-set check
	// catches a half-migrated fixture).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("fixture is not a JSON object: %v (raw %q)", err, raw)
	}
	for _, stale := range []string{"to", "expectedFrom"} {
		if _, ok := fields[stale]; ok {
			t.Fatalf("fixture carries stale 8.14a key %q — the apiserver decodes toState/fromState (ISI-4225)", stale)
		}
	}
	for _, want := range []string{"toState", "fromState"} {
		if _, ok := fields[want]; !ok {
			t.Fatalf("fixture is missing contract key %q (has %v)", want, fieldNames(fields))
		}
	}
	if len(fields) != 2 {
		t.Fatalf("fixture must carry exactly toState+fromState, has %v", fieldNames(fields))
	}

	// Replay the console's bytes through the real handler with a human session.
	store := &fakeTransitioner{result: coord.StateTransition{WorkItemID: "wi-contract", FromState: "todo", ToState: "in_progress"}}
	h := testStateServer(t, uuid.MustParse("44444444-4444-4444-4444-444444444444"), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-contract", string(raw), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("console body rejected: got %d, want 200 (body %s) — client/apiserver contract skew (ISI-4225)", rec.Code, rec.Body.String())
	}

	// The store must have received exactly what the fixture declares — same
	// bytes in, same lanes out, no silent key remapping.
	var want struct {
		ToState   string `json:"toState"`
		FromState string `json:"fromState"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("re-decode fixture: %v", err)
	}
	if !store.called || store.gotTarget != want.ToState || store.gotFrom != want.FromState {
		t.Fatalf("store call did not mirror the fixture body: %+v (want target=%q from=%q)", store, want.ToState, want.FromState)
	}
}

// TestWorkItemStateConsoleLegacySpellingRejected — the stale 8.14a client body
// {to, expectedFrom} must keep 400-ing with "toState required" (the exact
// failure seen in the M1.6 smoke): if this handler ever starts accepting the
// old spelling silently, the fixture-pinned contract above has been bypassed,
// not extended.
func TestWorkItemStateConsoleLegacySpellingRejected(t *testing.T) {
	store := &fakeTransitioner{}
	h := testStateServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, patchState("wi-legacy", `{"to":"in_progress","expectedFrom":"todo"}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("legacy 8.14a body: got %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "toState required") {
		t.Fatalf("legacy body error: got %q, want \"toState required\"", rec.Body.String())
	}
	if store.called {
		t.Fatal("store must not run for the legacy spelling")
	}
}

func fieldNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
