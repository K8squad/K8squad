package discussion

// Unit coverage for the /reset human-only guard (ADR-0030 §(a)7) — a PURE function (no I/O), the exact
// mirror of PartyStartAllowed, so it belongs in the default unit lane alongside the reader-filter
// integration tests in reset_integration_test.go. Agent-vs-human is DERIVED from the server-stamped
// provenance (auth.AgentID), never a body flag.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestResetAllowed(t *testing.T) {
	// Human (no agent id): allowed.
	human := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	if err := ResetAllowed(human); err != nil {
		t.Fatalf("ResetAllowed(human) = %v, want nil", err)
	}

	// Agent (agent id set): forbidden — an agent cannot wipe a thread.
	agent := "agent-7"
	run := "run-1"
	bot := AuthorContext{Principal: "agent:svc", TeamID: uuid.New(), AgentID: &agent, RunID: &run}
	if err := ResetAllowed(bot); err != ErrResetForbidden {
		t.Fatalf("ResetAllowed(agent) = %v, want ErrResetForbidden", err)
	}
}

// TestResetForbiddenMapsTo403 pins the store-error → HTTP-status contract: a forbidden reset is 403,
// the same bucket as ErrNotAuthor / ErrPartyStartForbidden.
func TestResetForbiddenMapsTo403(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStoreErr(rec, ErrResetForbidden)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeStoreErr(ErrResetForbidden): status = %d, want 403", rec.Code)
	}
}
