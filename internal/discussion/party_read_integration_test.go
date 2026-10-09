//go:build discussion_integration

// Apiserver integration coverage for the ISI-5617 terminal party-session reads (ISI-5613 Gap 2) against
// a REAL Postgres. Build-tag gated with the rest of the discussion integration lane; CI provisions
// Postgres and runs `go test -tags=discussion_integration ./internal/discussion/...`, and the suite
// SKIPS when DATABASE_URL is unset.
//
// It proves the contract ISI-5613's console depends on: once a debate closes, …/party-sessions/active
// 404s (unchanged), but GET …/party-sessions/{id} and GET …/party-sessions?includeClosed=true still
// return the TERMINAL PartySession carrying phase reason, final round count, and paid-run tally.
package discussion

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTerminalPartySessionReads walks a debate from open → close and asserts the terminal row stays
// reachable by id and via includeClosed, while …/active flips to 404 and the cross-thread id is a miss.
func TestTerminalPartySessionReads(t *testing.T) {
	db := openTestDB(t)
	applyMigration(t, db)
	// Layer 0033 (party_session) + 0034 (round-facilitator columns) onto the freshly-applied base
	// schema. 0034 is required because partySessionSelect (the read path this suite exercises) now
	// reads current_round_message_id / round_started_at (ISI-5615 WS-D.1).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"0033_discussion_party_session.sql", "0034_discussion_party_round_facilitator.sql"} {
		candidates := []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		}
		if d := os.Getenv("DISCUSSION_MIGRATIONS_DIR"); d != "" {
			candidates = append([]string{filepath.Join(d, name)}, candidates...)
		}
		var sqlBytes []byte
		var err error
		for _, c := range candidates {
			if sqlBytes, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if sqlBytes == nil {
			t.Fatalf("could not locate %s (tried %v); set DISCUSSION_MIGRATIONS_DIR", name, candidates)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("the shipped migration %s failed to apply against real Postgres: %v", name, err)
		}
	}

	srv := newServer(db)
	defer srv.Close()
	store := NewStore(db)

	team := uuid.New()
	project := uuid.New()
	base := srv.URL + "/api/projects/" + project.String() + "/discussion"

	// Open a thread (human) to hang the debate on.
	var opened postResp
	decode(t, do(t, mustReq(t, http.MethodPost, base+"/threads",
		`{"title":"storage debate","body":"kick off"}`, "henrik", team.String(), "", "")), &opened) //nolint:bodyclose
	thread := opened.ID

	// Open a party session (human-only). 201 + an active session with an id.
	var sess PartySession
	res := do(t, mustReq(t, http.MethodPost, base+"/threads/"+thread.String()+"/party-sessions",
		`{"body":"let's debate the storage approach with the team"}`, "henrik", team.String(), "", ""))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("start party session: status = %d, want 201", res.StatusCode)
	}
	decode(t, res, &sess) //nolint:bodyclose
	if sess.ID == uuid.Nil || sess.Phase != PartyPhaseActive {
		t.Fatalf("opened session = %+v, want a non-nil id in phase=active", sess)
	}

	// While active: get-by-id returns it, the default list returns exactly it, …/active returns it.
	if got := getSession(t, base, thread, sess.ID, team); got.Phase != PartyPhaseActive {
		t.Fatalf("get-by-id while active: phase = %q, want active", got.Phase)
	}
	if list := listSessions(t, base, thread, team, false); len(list) != 1 || list[0].ID != sess.ID {
		t.Fatalf("default list while active = %+v, want the one active session", list)
	}
	if ar := do(t, mustReq(t, http.MethodGet, base+"/threads/"+thread.String()+"/party-sessions/active",
		"", "henrik", team.String(), "", "")); ar.StatusCode != http.StatusOK {
		ar.Body.Close()
		t.Fatalf("…/active while live: status = %d, want 200", ar.StatusCode)
	} else {
		ar.Body.Close()
	}

	// Drive the session to a terminal state with real numbers: round 1, 4 paid runs, converged.
	if _, err := store.AdvanceRound(ctx, sess.ID, 0); err != nil {
		t.Fatalf("advance round: %v", err)
	}
	if _, err := store.RecordPaidRuns(ctx, sess.ID, 4); err != nil {
		t.Fatalf("record paid runs: %v", err)
	}
	if _, err := store.CloseSession(ctx, sess.ID, PartyPhaseConverged); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// …/active now 404s (live behaviour unchanged — a closed debate has no active session).
	if ar := do(t, mustReq(t, http.MethodGet, base+"/threads/"+thread.String()+"/party-sessions/active",
		"", "henrik", team.String(), "", "")); ar.StatusCode != http.StatusNotFound {
		ar.Body.Close()
		t.Fatalf("…/active after close: status = %d, want 404", ar.StatusCode)
	} else {
		ar.Body.Close()
	}

	// get-by-id STILL returns the terminal row with phase reason, round count, and paid-run tally.
	term := getSession(t, base, thread, sess.ID, team)
	if term.Phase != PartyPhaseConverged {
		t.Errorf("terminal phase = %q, want converged", term.Phase)
	}
	if term.Round != 1 {
		t.Errorf("terminal round = %d, want 1", term.Round)
	}
	if term.PaidRunsUsed != 4 {
		t.Errorf("terminal paidRunsUsed = %d, want 4", term.PaidRunsUsed)
	}
	if term.ClosedAt == nil {
		t.Errorf("terminal closedAt is nil, want a stamp")
	}

	// includeClosed=true returns the terminal session; the default list is now empty [].
	if list := listSessions(t, base, thread, team, true); len(list) != 1 || list[0].Phase != PartyPhaseConverged {
		t.Fatalf("includeClosed list = %+v, want the one converged session", list)
	}
	if list := listSessions(t, base, thread, team, false); len(list) != 0 {
		t.Fatalf("default list after close = %+v, want empty", list)
	}

	// A session id resolved against a DIFFERENT (in-scope) thread is a 404, not a cross-thread peek.
	var other postResp
	decode(t, do(t, mustReq(t, http.MethodPost, base+"/threads",
		`{"title":"other","body":"x"}`, "henrik", team.String(), "", "")), &other) //nolint:bodyclose
	if r := do(t, mustReq(t, http.MethodGet,
		base+"/threads/"+other.ID.String()+"/party-sessions/"+sess.ID.String(), "", "henrik", team.String(), "", "")); r.StatusCode != http.StatusNotFound {
		r.Body.Close()
		t.Fatalf("cross-thread get-by-id: status = %d, want 404", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// An unknown session id is a 404.
	if r := do(t, mustReq(t, http.MethodGet,
		base+"/threads/"+thread.String()+"/party-sessions/"+uuid.New().String(), "", "henrik", team.String(), "", "")); r.StatusCode != http.StatusNotFound {
		r.Body.Close()
		t.Fatalf("unknown get-by-id: status = %d, want 404", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

func getSession(t *testing.T, base string, thread, id uuid.UUID, team uuid.UUID) PartySession {
	t.Helper()
	res := do(t, mustReq(t, http.MethodGet, base+"/threads/"+thread.String()+"/party-sessions/"+id.String(),
		"", "henrik", team.String(), "", ""))
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("get-by-id %s: status = %d, want 200", id, res.StatusCode)
	}
	var s PartySession
	decode(t, res, &s) //nolint:bodyclose
	return s
}

func listSessions(t *testing.T, base string, thread, team uuid.UUID, includeClosed bool) []PartySession {
	t.Helper()
	url := base + "/threads/" + thread.String() + "/party-sessions"
	if includeClosed {
		url += "?includeClosed=true"
	}
	res := do(t, mustReq(t, http.MethodGet, url, "", "henrik", team.String(), "", ""))
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("list (includeClosed=%v): status = %d, want 200", includeClosed, res.StatusCode)
	}
	var out []PartySession
	decode(t, res, &out) //nolint:bodyclose
	return out
}
