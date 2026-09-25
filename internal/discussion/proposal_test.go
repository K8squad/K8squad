package discussion

// ISI-4928 — proposal kind unit tests (no DB): the payload contract, the wire handler's
// pre-store gates, and the lifecycle sentinel → HTTP mapping. The DB-backed lifecycle
// (CAS transitions, tenancy, post-back) runs in the integration lane.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestProposalPayloadValidate(t *testing.T) {
	cases := []struct {
		name    string
		payload ProposalPayload
		wantErr error // nil ⇒ must pass
	}{
		{"create_ticket ok", ProposalPayload{Action: ProposalActionCreateTicket, Title: "ship"}, nil},
		{"create_ticket needs title", ProposalPayload{Action: ProposalActionCreateTicket}, ErrInvalidProposalPayload},
		{"party_run ok", ProposalPayload{Action: ProposalActionPartyRun, Title: "party"}, nil},
		{"party_run needs title", ProposalPayload{Action: ProposalActionPartyRun}, ErrInvalidProposalPayload},
		{"assign_agent ok", ProposalPayload{Action: ProposalActionAssignAgent, TicketID: "wi-1", AssigneeAgentID: "a:kimi"}, nil},
		{"assign_agent needs ticket", ProposalPayload{Action: ProposalActionAssignAgent, AssigneeAgentID: "a:kimi"}, ErrInvalidProposalPayload},
		{"assign_agent needs agent", ProposalPayload{Action: ProposalActionAssignAgent, TicketID: "wi-1"}, ErrInvalidProposalPayload},
		{"unknown action", ProposalPayload{Action: "self_destruct"}, ErrInvalidProposalPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.payload.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestPostProposalUnauthenticated — the route rides the same BFFAuthz choke point; a request
// without a principal is refused before any store call (nil store proves it).
func TestPostProposalUnauthenticated(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"/api/projects/"+uuid.NewString()+"/discussion/threads/"+uuid.NewString()+"/proposals", nil)
	rec := httptest.NewRecorder()
	mountedRouter(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// TestPostProposalIgnoresBodyAuthor — the proposal wire struct carries NO author_* field, so a
// forged author in the body has no path into the stored row (AC3, structural).
func TestPostProposalIgnoresBodyAuthor(t *testing.T) {
	forged := `{"body":"propose","author_principal":"principal:victim","authorAgentId":"agent:evil",` +
		`"payload":{"action":"create_ticket","title":"t"}}`
	var req postProposalReq
	if err := json.Unmarshal([]byte(forged), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Payload.Action != ProposalActionCreateTicket || req.Payload.Title != "t" {
		t.Fatalf("payload did not decode: %+v", req.Payload)
	}
}

// TestWriteStoreErrProposalSentinels — the lifecycle sentinels map onto the room's error contract:
// tenancy/invisible ⇒ 404, bad payload ⇒ 400, already-decided ⇒ 409.
func TestWriteStoreErrProposalSentinels(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrProposalNotFound, http.StatusNotFound},
		{ErrInvalidProposalPayload, http.StatusBadRequest},
		{ErrProposalNotProposed, http.StatusConflict},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeStoreErr(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%v: got %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}
