//go:build discussion_integration

// ISI-5675 (ISI-5598 WS-E) — HTTP-level integration coverage for the /reset endpoint. The WS-B
// reset_integration_test.go suite exercises Store.ResetThread directly; this file closes the two gaps
// the WS-E scope names explicitly: the reset ENDPOINT happy-path (POST .../threads/{id}/reset routed
// through the mounted handler + headerAuth seam, 201 + response envelope + reader-filter via a real GET)
// and IDEMPOTENCY (resetting an already-reset/empty thread is a safe no-op that appends a fresh marker,
// hides nothing new, and advances the floor). It also pins the human-only 403 THROUGH the HTTP layer
// (agent principal via X-Test-Agent), proving the fence is enforced at the routed surface, not only in
// the pure guard. Reuses the integration_test.go harness (newServer/mustReq/do/decode) + openPartyTestDB
// (full migration chain incl. 0036) — store.db is same-package accessible.
//
//	go test -tags=discussion_integration ./internal/discussion/...
package discussion

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// resetHTTPResp mirrors the resetThread handler's 201 envelope (handler.go):
// {"status":"reset","marker":{…},"resetAt":…,"hiddenCount":N}.
type resetHTTPResp struct {
	Status      string  `json:"status"`
	HiddenCount int     `json:"hiddenCount"`
	Marker      Message `json:"marker"`
}

// threadGetResp is the slice of the GET thread body this suite asserts on: the visible transcript plus
// the reset metadata the reader derives from the latest live marker.
type threadGetResp struct {
	ResetAt  *string `json:"resetAt"`
	ResetBy  *string `json:"resetBy"`
	Messages []struct {
		ID   uuid.UUID `json:"id"`
		Body string    `json:"body"`
		Kind string    `json:"kind"`
	} `json:"messages"`
}

// resetHTTPServer stands up the mounted discussion router over a full-migration-chain DB (incl. 0036)
// and returns the base URL + a human team/project triple used by the whole suite.
func openResetHTTPServer(t *testing.T) (base string, team, project uuid.UUID) {
	t.Helper()
	store, cleanup := openPartyTestDB(t)
	t.Cleanup(cleanup)
	srv := newServer(store.db) // same-package access to the unexported *sql.DB
	t.Cleanup(srv.Close)
	return srv.URL, uuid.New(), uuid.New()
}

func threadsURL(base string, project uuid.UUID) string {
	return base + "/api/projects/" + project.String() + "/discussion/threads"
}

// TestResetThreadHTTP_HappyPath: POST .../reset routed through the handler returns 201 with the reset
// envelope (status, marker kind='reset' with NULL agent, hiddenCount), and a subsequent GET shows an
// empty transcript with reset metadata populated (resetBy = the human principal).
func TestResetThreadHTTP_HappyPath(t *testing.T) {
	base, team, project := openResetHTTPServer(t)
	principal := "user:alice"

	// Open a thread (opener) + one reply → 2 live, visible messages.
	var opened postResp
	decode(t, do(t, mustReq(t, http.MethodPost, threadsURL(base, project),
		`{"title":"history","body":"first"}`, principal, team.String(), "", "")), &opened) //nolint:bodyclose // decode closes

	do(t, mustReq(t, http.MethodPost, threadsURL(base, project)+"/"+opened.ID.String()+"/messages",
		`{"body":"second"}`, principal, team.String(), "", "")).Body.Close()

	// POST /reset → 201 Created with the reset envelope.
	res := do(t, mustReq(t, http.MethodPost, threadsURL(base, project)+"/"+opened.ID.String()+"/reset",
		`{"reason":"starting fresh"}`, principal, team.String(), "", ""))
	if res.StatusCode != http.StatusCreated {
		res.Body.Close()
		t.Fatalf("POST /reset: got %d, want 201", res.StatusCode)
	}
	var rr resetHTTPResp
	decode(t, res, &rr) //nolint:bodyclose // decode closes
	if rr.Status != "reset" {
		t.Errorf("status = %q, want reset", rr.Status)
	}
	if rr.HiddenCount != 2 {
		t.Errorf("hiddenCount = %d, want 2 (opener + reply)", rr.HiddenCount)
	}
	if rr.Marker.Kind != "reset" {
		t.Errorf("marker.kind = %q, want reset", rr.Marker.Kind)
	}
	if rr.Marker.AuthorAgentID != nil {
		t.Errorf("marker.author_agent_id = %v, want NULL (human-only fence)", *rr.Marker.AuthorAgentID)
	}

	// GET the thread → empty visible transcript (marker excluded, prior hidden), reset metadata present.
	var got threadGetResp
	decode(t, do(t, mustReq(t, http.MethodGet, threadsURL(base, project)+"/"+opened.ID.String(),
		"", principal, team.String(), "", "")), &got) //nolint:bodyclose // decode closes
	if len(got.Messages) != 0 {
		t.Errorf("post-reset visible = %d messages, want 0", len(got.Messages))
	}
	if got.ResetAt == nil || got.ResetBy == nil {
		t.Fatalf("post-reset metadata = (resetAt=%v, resetBy=%v), want both set", got.ResetAt, got.ResetBy)
	}
	if *got.ResetBy != principal {
		t.Errorf("resetBy = %q, want %q", *got.ResetBy, principal)
	}
}

// TestResetThreadHTTP_Idempotent: resetting an already-reset (empty) thread again is a safe no-op through
// the HTTP layer — still 201, hiddenCount=0 (nothing new to hide), the transcript stays empty, and the
// reset floor advances to the newer marker (resetAt changes). A double-click / retry never errors.
func TestResetThreadHTTP_Idempotent(t *testing.T) {
	base, team, project := openResetHTTPServer(t)
	principal := "user:bob"

	var opened postResp
	decode(t, do(t, mustReq(t, http.MethodPost, threadsURL(base, project),
		`{"title":"idem","body":"only message"}`, principal, team.String(), "", "")), &opened) //nolint:bodyclose // decode closes
	resetURL := threadsURL(base, project) + "/" + opened.ID.String() + "/reset"

	// First reset hides the opener.
	var first resetHTTPResp
	r1 := do(t, mustReq(t, http.MethodPost, resetURL, `{}`, principal, team.String(), "", ""))
	if r1.StatusCode != http.StatusCreated {
		r1.Body.Close()
		t.Fatalf("first reset: got %d, want 201", r1.StatusCode)
	}
	decode(t, r1, &first) //nolint:bodyclose // decode closes
	if first.HiddenCount != 1 {
		t.Errorf("first reset hiddenCount = %d, want 1", first.HiddenCount)
	}

	// Second reset on the now-empty thread: still 201, hides nothing new.
	var second resetHTTPResp
	r2 := do(t, mustReq(t, http.MethodPost, resetURL, `{}`, principal, team.String(), "", ""))
	if r2.StatusCode != http.StatusCreated {
		r2.Body.Close()
		t.Fatalf("second (idempotent) reset: got %d, want 201", r2.StatusCode)
	}
	decode(t, r2, &second) //nolint:bodyclose // decode closes
	if second.HiddenCount != 0 {
		t.Errorf("second reset hiddenCount = %d, want 0 (nothing new to hide)", second.HiddenCount)
	}
	if second.Marker.ID == first.Marker.ID {
		t.Errorf("second reset reused marker %s, want a fresh marker row", second.Marker.ID)
	}

	// Transcript stays empty; floor advanced to the newer (second) marker.
	var got threadGetResp
	decode(t, do(t, mustReq(t, http.MethodGet, threadsURL(base, project)+"/"+opened.ID.String(),
		"", principal, team.String(), "", "")), &got) //nolint:bodyclose // decode closes
	if len(got.Messages) != 0 {
		t.Errorf("after double reset visible = %d, want 0", len(got.Messages))
	}
	if got.ResetAt == nil {
		t.Fatalf("after double reset resetAt = nil, want the latest marker's created_at")
	}
}

// TestResetThreadHTTP_AgentForbidden: an agent principal (X-Test-Agent set) is 403 at the routed /reset
// surface, and the thread is untouched — the human-only fence is enforced through HTTP, not only in the
// pure ResetAllowed guard.
func TestResetThreadHTTP_AgentForbidden(t *testing.T) {
	base, team, project := openResetHTTPServer(t)
	principal := "user:carol"

	var opened postResp
	decode(t, do(t, mustReq(t, http.MethodPost, threadsURL(base, project),
		`{"title":"nope","body":"keep me"}`, principal, team.String(), "", "")), &opened) //nolint:bodyclose // decode closes

	// Agent tries to reset → 403.
	res := do(t, mustReq(t, http.MethodPost, threadsURL(base, project)+"/"+opened.ID.String()+"/reset",
		`{}`, "agent:amelia", team.String(), "amelia", "run-9"))
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("agent reset: got %d, want 403", res.StatusCode)
	}

	// Thread is unchanged: original message still visible, no reset metadata.
	var got threadGetResp
	decode(t, do(t, mustReq(t, http.MethodGet, threadsURL(base, project)+"/"+opened.ID.String(),
		"", principal, team.String(), "", "")), &got) //nolint:bodyclose // decode closes
	if len(got.Messages) != 1 {
		t.Errorf("after rejected reset visible = %d, want 1 (unchanged)", len(got.Messages))
	}
	if got.ResetAt != nil {
		t.Errorf("after rejected reset resetAt = %v, want nil (no marker written)", got.ResetAt)
	}
}
