// inbox.go — ISI-5535 (E1 of ISI-5531): GET /api/squad/inbox + POST /api/squad/inbox/seen.
//
// Inbox ("Needs Human Decision") unions open in_review work items + open proposals across the
// caller's team (admin → fleet), ordered by last run's claimedAt descending. Decision_request
// rows are added in ISI-5536 (E2); the response shape is stable across E1→E2.
//
// ADR-0026 §3 specifies the two-data-plane join: Postgres supplies the item corpus; the
// controller-runtime informer cache (via SquadOverviewReader.Overview) supplies run claimedAt
// for ordering AND the Project UID→"namespace/name" index needed to build the click-through
// projectId. The join happens in Go, not SQL — exactly the discipline liveIssueIds.ts uses.
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Narrow store interfaces (seams for testing, wired from server.go Options)
// ============================================================================

// ReviewItemReader is the coord.WorkItemReadStore seam for inbox in_review arm.
type ReviewItemReader interface {
	ListReviewItems(ctx context.Context, teamID string) ([]coord.ReviewItem, error)
}

// OpenProposalReader is the discussion.Store seam for inbox proposals arm.
type OpenProposalReader interface {
	ListOpenProposalsForTeam(ctx context.Context, teamID string) ([]discussion.OpenProposalSummary, error)
}

// OpenDecisionReader is the discussion.Store seam for the inbox decision_request arm (ISI-5536 BE-7).
type OpenDecisionReader interface {
	ListOpenDecisionRequestsForTeam(ctx context.Context, teamID string) ([]discussion.OpenDecisionSummary, error)
}

// ============================================================================
// Response shapes (ADR-0026 §3.4, stable across E1→E2)
// ============================================================================

// InboxItem is one row of the GET /api/squad/inbox response. The key (union-member id) is
// stable: inReview:{workItemId} or proposal:{messageId}. DecisionType maps to chip hues:
// review→violet, proposal→green (approve), choose_one/choose_many→blue, free_form→amber.
type InboxItem struct {
	Key           string     `json:"key"`
	TicketID      string     `json:"ticketId,omitempty"` // work_item UUID; empty for create/party_run proposals
	ProjectID     string     `json:"projectId"`          // "namespace/name"; empty when unresolvable
	Title         string     `json:"title"`
	DecisionType  string     `json:"decisionType"` // review | proposal | approve | choose_one | choose_many | free_form
	RaisedByAgent string     `json:"raisedByAgent,omitempty"`
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"` // nil ⇒ no run yet; row orders by updatedAt
	Unread        bool       `json:"unread"`
	Live          bool       `json:"live,omitempty"` // ISI-5528 live-run marker
}

// InboxResponse is the top-level GET /api/squad/inbox envelope (ADR-0026 §3.4).
// Items is never nil (empty ⇒ [], not null) so the console renders the empty state.
type InboxResponse struct {
	Fleet bool        `json:"fleet,omitempty"`
	Items []InboxItem `json:"items"`
}

// ============================================================================
// Handlers
// ============================================================================

// squadInbox is the handler behind GET /api/squad/inbox. It rides the §13 BFF authz choke
// point (mounted in routes), so the AuthorContext is already stamped on the request context.
// The response is an InboxResponse with Fleet+Items (never-nil array). Admin→fleet-wide.
func (s *Server) squadInbox(
	reviews ReviewItemReader,
	proposals OpenProposalReader,
	decisions OpenDecisionReader, // may be nil ⇒ decision arm contributes zero rows (E1 degrade)
	markers ReadMarkerStore, // may be nil ⇒ all-unread
	overview SquadOverviewReader,
	_ ProjectRefResolver, // reserved for future cross-project project-path resolution
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		teamID := authTeamScope(r)

		// --- Cache arm: run claimedAt for ordering + UID→project path index ---
		sq, err := overview.Overview(r.Context(), auth.TeamID.String(), auth.IsAdmin)
		if err != nil && !errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusBadGateway, "inbox cache read unavailable")
			return
		}

		// Build UID→"namespace/name" index and workItem→latestClaimedAt + live maps.
		uidToPath := make(map[string]string, len(sq.Projects))
		runClaimedAt := make(map[string]*time.Time) // workItemID → max claimedAt
		liveByItem := make(map[string]bool)
		for _, p := range sq.Projects {
			if p.UID != "" {
				if p.Namespace != "" {
					uidToPath[p.UID] = p.Namespace + "/" + p.Name
				} else {
					uidToPath[p.UID] = p.Name
				}
			}
			for _, rs := range p.Runs {
				if rs.WorkItem == "" {
					continue
				}
				if rs.ClaimedAt != nil {
					existing := runClaimedAt[rs.WorkItem]
					if existing == nil || rs.ClaimedAt.After(*existing) {
						t := *rs.ClaimedAt
						runClaimedAt[rs.WorkItem] = &t
					}
				}
				if inboxIsLivePhase(rs.Phase) {
					liveByItem[rs.WorkItem] = true
				}
			}
		}

		// --- Postgres arms (best-effort: one arm failure logs + contributes zero rows, so the
		// other arm's rows still render — the same degrade-don't-fail posture squad-overview uses) ---
		reviewItems, err := reviews.ListReviewItems(r.Context(), teamID)
		if err != nil {
			log.Printf("apiserver: inbox: ListReviewItems: %v", err)
			reviewItems = nil
		}

		proposalItems, err := proposals.ListOpenProposalsForTeam(r.Context(), teamID)
		if err != nil {
			log.Printf("apiserver: inbox: ListOpenProposalsForTeam: %v", err)
			proposalItems = nil
		}

		// Decision_request arm (ISI-5536 BE-7). Nil reader (E1-only deployment) ⇒ zero rows.
		var decisionItems []discussion.OpenDecisionSummary
		if decisions != nil {
			decisionItems, err = decisions.ListOpenDecisionRequestsForTeam(r.Context(), teamID)
			if err != nil {
				log.Printf("apiserver: inbox: ListOpenDecisionRequestsForTeam: %v", err)
				decisionItems = nil
			}
		}

		// --- Read-marker arm (best-effort: nil store or error ⇒ all unread) ---
		var seenKeys map[string]time.Time
		if markers != nil {
			keys := make([]string, 0, len(reviewItems)+len(proposalItems)+len(decisionItems))
			for _, it := range reviewItems {
				keys = append(keys, "inReview:"+it.ID)
			}
			for _, ps := range proposalItems {
				keys = append(keys, "proposal:"+ps.MessageID)
			}
			for _, ds := range decisionItems {
				keys = append(keys, "decision:"+ds.MessageID)
			}
			var mErr error
			seenKeys, mErr = markers.Seen(r.Context(), auth.Principal, keys)
			if mErr != nil {
				log.Printf("apiserver: inbox: read-marker Seen: %v", mErr) // degrade to all-unread
			}
		}

		// --- Join + assemble rows ---
		type row struct {
			item     InboxItem
			orderKey time.Time
		}
		var rows []row

		for _, it := range reviewItems {
			key := "inReview:" + it.ID
			orderKey := it.UpdatedAt
			if t := runClaimedAt[it.ID]; t != nil {
				orderKey = *t
			}
			unread := true
			if seenKeys != nil {
				if seenAt, seen := seenKeys[key]; seen && !orderKey.After(seenAt) {
					unread = false
				}
			}
			rows = append(rows, row{
				item: InboxItem{
					Key:           key,
					TicketID:      it.ID,
					ProjectID:     uidToPath[it.ProjectID],
					Title:         it.Title,
					DecisionType:  "review",
					RaisedByAgent: it.Assignee,
					LastRunAt:     runClaimedAt[it.ID],
					Unread:        unread,
					Live:          liveByItem[it.ID],
				},
				orderKey: orderKey,
			})
		}

		for _, ps := range proposalItems {
			key := "proposal:" + ps.MessageID
			orderKey := ps.CreatedAt
			if ps.TicketID != "" {
				if t := runClaimedAt[ps.TicketID]; t != nil {
					orderKey = *t
				}
			}
			title := ps.Body
			if ps.Title != "" {
				title = ps.Title
			}
			var lastRunAt *time.Time
			if ps.TicketID != "" {
				lastRunAt = runClaimedAt[ps.TicketID]
			}
			live := ps.TicketID != "" && liveByItem[ps.TicketID]
			unread := true
			if seenKeys != nil {
				if seenAt, seen := seenKeys[key]; seen && !orderKey.After(seenAt) {
					unread = false
				}
			}
			rows = append(rows, row{
				item: InboxItem{
					Key:           key,
					TicketID:      ps.TicketID,
					ProjectID:     ps.ProjectUID, // thread.project_id is already "ns/name" text (migration 0026)
					Title:         title,
					DecisionType:  "proposal",
					RaisedByAgent: ps.AuthorAgent,
					LastRunAt:     lastRunAt,
					Unread:        unread,
					Live:          live,
				},
				orderKey: orderKey,
			})
		}

		// Decision_request arm (ISI-5536 BE-7, ADR-0026 §3.3/§3.4). decisionType = the card's mode
		// (approve→green, choose_one/choose_many→blue, free_form→amber); the card binds to its
		// work_item (ds.TicketID) which carries the run-join ordering + live marker, same as proposals.
		for _, ds := range decisionItems {
			key := "decision:" + ds.MessageID
			orderKey := ds.CreatedAt
			var lastRunAt *time.Time
			live := false
			if ds.TicketID != "" {
				if t := runClaimedAt[ds.TicketID]; t != nil {
					orderKey = *t
					lastRunAt = t
				}
				live = liveByItem[ds.TicketID]
			}
			decisionType := ds.Mode
			if decisionType == "" {
				decisionType = "decision"
			}
			unread := true
			if seenKeys != nil {
				if seenAt, seen := seenKeys[key]; seen && !orderKey.After(seenAt) {
					unread = false
				}
			}
			rows = append(rows, row{
				item: InboxItem{
					Key:           key,
					TicketID:      ds.TicketID,
					ProjectID:     ds.ProjectUID, // thread.project_id is already "ns/name" text
					Title:         ds.Title,
					DecisionType:  decisionType,
					RaisedByAgent: ds.AuthorAgent,
					LastRunAt:     lastRunAt,
					Unread:        unread,
					Live:          live,
				},
				orderKey: orderKey,
			})
		}

		// Sort by orderKey desc, key asc (deterministic tiebreak, ADR-0026 §3.3).
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].orderKey.Equal(rows[j].orderKey) {
				return rows[i].orderKey.After(rows[j].orderKey)
			}
			return rows[i].item.Key < rows[j].item.Key
		})

		items := make([]InboxItem, len(rows))
		for i, row := range rows {
			items[i] = row.item
		}

		writeJSON(w, http.StatusOK, InboxResponse{
			Fleet: sq.Fleet || auth.IsAdmin,
			Items: items,
		})
	}
}

// squadInboxSeen handles POST /api/squad/inbox/seen — upserts read-markers for the supplied keys.
// Body: {"keys":["inReview:…","proposal:…"]}. Idempotent; 200 on success. 400 on bad body.
func (s *Server) squadInboxSeen(markers ReadMarkerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var body struct {
			Keys []string `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Keys) == 0 {
			writeJSONError(w, http.StatusBadRequest, "keys required")
			return
		}
		if err := markers.MarkSeen(r.Context(), auth.Principal, body.Keys); err != nil {
			writeJSONError(w, http.StatusBadGateway, "mark-seen failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// inboxIsLivePhase mirrors the frontend's isLiveRunPhase (lib/tickets/liveIssueIds.ts): a run
// is live when it is actively running (Running/Claiming/Dispatching/Collecting) OR waiting
// to start (Queued/Waiting prefix). Keep semantically identical to the frontend constant.
func inboxIsLivePhase(phase string) bool {
	switch phase {
	case "Running", "Claiming", "Dispatching", "Collecting":
		return true
	}
	p := strings.ToLower(phase)
	return strings.HasPrefix(p, "queued") || strings.HasPrefix(p, "waiting")
}
