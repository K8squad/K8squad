// Package reviewdispatch is the operator-side composition of the ISI-4750 E4
// system PR-review dispatch (ISI-4776). It binds the pure decision core in
// pkg/controller/reviewtrigger to the three concrete authorities it depends on —
// the standing policy (from the Project CR), team membership (from the Team CR
// composition), and the custody-wall-sensitive work-item create + dispatch (coord
// under a SYSTEM identity) — and hands the assembled reviewtrigger.Dispatcher to
// the repo-sync reconciler.
//
// This is the highest-risk authz path in the epic (ISI-4766). The invariants it
// must uphold, all verified by the unit tests here and gated on adversarial Code
// Reviewer + Testing Architect review:
//   - NO agent-authored work item: the review item is created under the SYSTEM
//     principal reviewtrigger.Initiator, never an agent identity (ISI-4711 wall).
//   - SYSTEM dispatch provenance: the coord dispatch is stamped
//     Initiator=reviewtrigger.Initiator and Principal=the human EnabledBy — never
//     spoofed as a human or an agent dispatch (D1).
//   - EnabledBy provenance is threaded and REQUIRED: an enabled policy with no
//     server-stamped authorizing principal is a misconfiguration, not a silent
//     unprovenanced dispatch.
//   - dedup idempotent under concurrent reconciles: create-if-absent is
//     serialised in coord (EnsureReviewWorkItem's advisory lock).
package reviewdispatch

import (
	"context"

	"github.com/K8squad/K8squad/pkg/coord"
)

// TeamMembership implements reviewtrigger.TeamMembership for scope=team_authored.
// It answers "is this PR actor an agent in the owning Team's composition?" by
// reusing the EXACT coord.TeamAgentResolver the board dispatch op authorizes
// against (the shared informer-cache resolver keyed on the Team CR uid = coord
// team_id), so the review scope filter and the dispatch authorization can never
// disagree about a Team's composition.
type TeamMembership struct {
	resolver coord.TeamAgentResolver
}

// NewTeamMembership binds the membership check to a coord.TeamAgentResolver. The
// resolver is required.
func NewTeamMembership(resolver coord.TeamAgentResolver) *TeamMembership {
	return &TeamMembership{resolver: resolver}
}

// IsTeamAgent reports whether actor is a named agent in the Team whose uid is
// teamID. An empty teamID or actor is a clean "not a member" (no lookup); a
// resolver error — including a dangling team uid — is propagated so the reconcile
// fails loudly rather than vacuously treating every PR as out-of-scope.
func (m *TeamMembership) IsTeamAgent(ctx context.Context, teamID, actor string) (bool, error) {
	if teamID == "" || actor == "" {
		return false, nil
	}
	agents, err := m.resolver.TeamAgents(ctx, teamID)
	if err != nil {
		return false, err
	}
	for _, a := range agents {
		if a == actor {
			return true, nil
		}
	}
	return false, nil
}
