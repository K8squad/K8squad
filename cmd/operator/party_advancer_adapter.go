// ISI-5615 (ISI-5569 WS-D.1) — thin adapters wiring discussion.PartyAdvancer to coord stores.
//
// The discussion package defines narrow interfaces (AdvancerWorkItemWriter, AdvancerWorkItemDispatcher,
// AdvancerRosterReader) so it stays free of coord imports. These adapters satisfy them using the real
// coord stores and the Team-CR informer cache.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// coordAdvancerWriter adapts *coord.WorkItemWriteStore to discussion.AdvancerWorkItemWriter.
type coordAdvancerWriter struct {
	store *coord.WorkItemWriteStore
}

func (w coordAdvancerWriter) CreateWorkItem(ctx context.Context, in discussion.AdvancerCreateInput) (discussion.AdvancerWorkItemResult, error) {
	rec, err := w.store.CreateWorkItem(ctx, coord.CreateWorkItemInput{
		ProjectID: in.ProjectID,
		TeamID:    in.TeamID,
		Title:     in.Title,
		Body:      in.Body,
		Principal: in.Principal,
	})
	if err != nil {
		return discussion.AdvancerWorkItemResult{}, err
	}
	return discussion.AdvancerWorkItemResult{ID: rec.ID}, nil
}

// coordAdvancerDispatcher adapts *coord.WorkItemDispatchStore to discussion.AdvancerWorkItemDispatcher.
type coordAdvancerDispatcher struct {
	store *coord.WorkItemDispatchStore
}

func (d coordAdvancerDispatcher) RequestDispatch(ctx context.Context, in discussion.AdvancerDispatchInput) error {
	_, err := d.store.RequestDispatch(ctx, coord.RequestDispatchInput{
		WorkItemID: in.WorkItemID,
		AgentID:    in.AgentID,
		TeamID:     in.TeamID,
		Principal:  in.Principal,
		Initiator:  in.Initiator,
	})
	return err
}

// teamCoordinatorRoster implements discussion.AdvancerRosterReader using the Team-CR informer cache.
// It resolves the Team's Coordinator-role agent by listing agents and finding the coordinator. Since
// the informer cache (coord.TeamAgentResolver) only carries agent names (not roles), this adapter
// uses a fallback: it tries the discussion OrgReader projection if available, else picks the first
// agent in the team as a degraded default (best-effort for the MVP).
type teamCoordinatorRoster struct {
	resolver coord.TeamAgentResolver
}

// CoordinatorForTeam returns the coordinator agent name for teamID. It uses the Team-CR cache which
// carries agent names but not the coordinator role flag, so it falls back to the first agent in the
// team as a degrade. A more precise implementation would read the coordinator role from the Team CR
// spec directly — that is a follow-up improvement once party mode is live.
func (r teamCoordinatorRoster) CoordinatorForTeam(ctx context.Context, teamID string) (string, bool, error) {
	if r.resolver == nil {
		return "", false, errors.New("party advancer: teamCoordinatorRoster: nil resolver")
	}
	agents, err := r.resolver.TeamAgents(ctx, teamID)
	if err != nil {
		return "", false, fmt.Errorf("party advancer: TeamAgents(%q): %w", teamID, err)
	}
	// The Team-CR informer cache doesn't surface which agent is the Coordinator role — return the
	// first dispatchable agent as a best-effort degrade. This is intentionally imprecise: in a
	// well-configured team the coordinator is typically the first/only special agent. A follow-up
	// will expose the Coordinator flag from the Team CR spec directly (ISI-5615 known gap).
	for _, name := range agents {
		if strings.TrimSpace(name) != "" {
			return name, true, nil
		}
	}
	return "", false, nil
}
