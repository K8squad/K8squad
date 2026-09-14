package apiserver

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
)

// teamagentresolve.go — the production binding of coord.TeamAgentResolver
// (ADR-0022 / ISI-4411): resolve a Team CR's agent composition by its uid so the
// board dispatch op can enforce agent-∈-Team (§D4) without pulling Kubernetes
// into pkg/coord. Same informer-cache reader the project-ref resolver and the
// read models use, so dispatch resolves a Team identically to its siblings.

// clientTeamAgentResolver resolves Team.Spec.Agents through the shared informer
// cache — an in-memory list, so it is safe to call inside the dispatch txn.
type clientTeamAgentResolver struct {
	reader client.Reader
}

// NewClientTeamAgentResolver binds the resolver to the host's informer cache.
func NewClientTeamAgentResolver(reader client.Reader) coord.TeamAgentResolver {
	return clientTeamAgentResolver{reader: reader}
}

// TeamAgents lists the agent names of the Team whose CR uid is teamUID
// (coord.work_item.team_id). A uid that resolves to no Team CR is an error, not a
// silent empty set: dispatch must fail loudly on a dangling team rather than
// vacuously reject every agent as "not a member".
func (r clientTeamAgentResolver) TeamAgents(ctx context.Context, teamUID string) ([]string, error) {
	if teamUID == "" {
		return nil, fmt.Errorf("apiserver.TeamAgents: empty team uid")
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return nil, fmt.Errorf("apiserver.TeamAgents: list teams: %w", err)
	}
	for i := range teams.Items {
		t := &teams.Items[i]
		if string(t.UID) == teamUID {
			return objectRefNames(t.Spec.Agents), nil
		}
	}
	return nil, fmt.Errorf("apiserver.TeamAgents: team uid %s resolves to no Team CR", teamUID)
}
