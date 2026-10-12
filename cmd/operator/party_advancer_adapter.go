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
	"log/slog"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
	"github.com/K8squad/K8squad/pkg/coord"
)

// coordAdvancerWriter adapts *coord.WorkItemWriteStore to discussion.AdvancerWorkItemWriter.
//
// ISI-5680 (ISI-5662 WS3): the advancer hands CreateWorkItem the party session's ProjectID, which is the
// room's "namespace/name" slug (discussion.party_session.project_id is TEXT — migration 0026). But
// coord.CreateWorkItem inserts project_id via $1::uuid, so a slug casts to uuid and ERRORS ("invalid input
// syntax for type uuid") — the facilitator/takeaways mint silently fails and nothing is dispatched. We
// therefore resolve the slug to the coord Project UID (+ owning Team UID) here, exactly as the apiserver
// (internal/apiserver/mentiondispatch.go) and the sequential VoiceDispatcher (internal/mentiondispatch)
// do before they call the same coord store. A UID-bearing ProjectID passes through unchanged.
type coordAdvancerWriter struct {
	store    *coord.WorkItemWriteStore
	resolver mentiondispatch.ProjectResolver
	// markSource flips the just-minted facilitator/takeaways work item's source to 'discussion' (ISI-5692).
	//
	// This is what makes the facilitator's minted Run a DISCUSSION run, and it is the whole fix for the
	// ISI-5662 WS3 stall. The operator assembler injects the built-in memory-discussion endpoint — the
	// discussion_post tool + a discussion capability token carrying capability.CapabilityCoordinator (when
	// the dispatched agent holds the Team Coordinator role) — ONLY for a source=discussion run
	// (pkg/capability.isDiscussionRun keys on the api.LabelWorkItemSource label the intake sweep stamps from
	// THIS column, rundrive/intake.go). Without the flip the advancer mints a plain board item → an INTAKE
	// run whose capability manifest has only work_item_* tools and NO discussion endpoint, so the
	// coordinator cannot post its round directive through the MCP discussion_post edge ISI-5688 fixed; the
	// directive never stamps RoundVoices and the advancer has no roster to sequence → the round stalls with
	// zero voices (the live-smoke refutation ISI-5692 filed against ISI-5688).
	//
	// Kept as a direct UPDATE at this edge (NOT a new coord.CreateWorkItem parameter), mirroring
	// internal/mentiondispatch.markSourceDiscussion — the voice-run mint seam that already board-hides its
	// thread-runs the same way — so the create seam is reused verbatim and no new pkg/coord export is
	// introduced (FR-B3). It runs inside CreateWorkItem, before the advancer's RequestDispatch moves the
	// item onto the dispatch lane ('todo'), so the source is already 'discussion' by the time the intake
	// sweep can select it — no window where the facilitator item surfaces as a board item or mints as an
	// intake run. nil leaves the behaviour unchanged (an intake-sourced mint), so a test writer can omit it.
	markSource func(ctx context.Context, workItemID string) error
}

func (w coordAdvancerWriter) CreateWorkItem(ctx context.Context, in discussion.AdvancerCreateInput) (discussion.AdvancerWorkItemResult, error) {
	projectID := in.ProjectID
	teamID := in.TeamID
	if w.resolver != nil {
		resolved, rerr := w.resolver.ResolveProject(ctx, in.ProjectID)
		if rerr != nil {
			return discussion.AdvancerWorkItemResult{}, fmt.Errorf("party advancer: resolve project %q: %w", in.ProjectID, rerr)
		}
		projectID = resolved.UID
		// Prefer the resolved owning-Team UID when present; the session's TeamID is already a UID, so a
		// resolver that can't name the team (TeamUID == "") leaves the caller's value intact.
		if resolved.TeamUID != "" {
			teamID = resolved.TeamUID
		}
	}
	rec, err := w.store.CreateWorkItem(ctx, coord.CreateWorkItemInput{
		ProjectID: projectID,
		TeamID:    teamID,
		Title:     in.Title,
		Body:      in.Body,
		Principal: in.Principal,
	})
	if err != nil {
		return discussion.AdvancerWorkItemResult{}, err
	}
	// Flip the item to source=discussion so its Run is assembled as a discussion run (see markSource). A
	// failure here is fatal to the mint: an intake-sourced facilitator cannot post its directive through the
	// discussion_post edge, so proceeding would silently reproduce the ISI-5692 zero-voice stall — surface
	// it so the advancer logs and the next tick retries rather than dispatch a structurally-wrong run.
	if w.markSource != nil {
		if serr := w.markSource(ctx, rec.ID); serr != nil {
			return discussion.AdvancerWorkItemResult{}, fmt.Errorf("party advancer: mark facilitator item %s source=discussion: %w", rec.ID, serr)
		}
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

// teamCoordinatorRoster implements discussion.AdvancerRosterReader by resolving the REAL Coordinator-role
// agent of a Team from the CRs (ISI-5669 WS1, root-cause B). It reads the Team's agent composition, each
// agent's referenced Role, and the Role.Spec.Coordinator flag through a cache-backed client.Reader (the
// operator's mgr.GetClient()) — the exact Agent→Role→coordinator resolution the apiserver's roster
// projection does (internal/apiserver/fleetlist.go Agents, cmd/apiserver hasCoordinatorRole).
//
// Why the flag matters: the advancer dispatches each round's facilitator run to whatever name this
// returns. The mint-gate that turns the facilitator's @-mention post into a sequenced round
// (internal/discussion/party_dispatch.go coordinatorOf) ONLY fires when the posting agent is the Team's
// real Coordinator-role agent. The prior implementation returned the FIRST agent (the informer cache
// carried names only), so the facilitator ran as a non-coordinator, coordinatorOf rejected its post, and
// the handler fell through to the parallel DispatchMentionsFrom broadcast — the exact N-way fan-out
// ISI-5662 reported. Resolving the real coordinator keeps the two seams in agreement so the round
// sequences.
//
// No-coordinator-in-room case (fail safe, not silent broadcast): a Team with no Coordinator-role agent
// returns ok=false, which the advancer (mintRound) logs and declines to mint — the session closes on the
// facilitator-post timeout rather than dispatching the facilitator to an arbitrary agent whose post would
// then broadcast. A dangling/absent Team uid is a hard error, surfaced not swallowed.
type teamCoordinatorRoster struct {
	reader client.Reader
}

// CoordinatorForTeam returns the name of teamUID's Coordinator-role agent. It lists the Team by uid for
// its namespace + agent refs, indexes the namespace's Coordinator-role names, and returns the first team
// agent whose referenced Role carries Coordinator=true (team @-mention / roster order). ok=false with a
// nil error means the Team configures no Coordinator-role agent — an explicit fail-safe the caller treats
// as "do not mint", never a degrade to an arbitrary agent. A uid that resolves to no Team CR is an error.
func (r teamCoordinatorRoster) CoordinatorForTeam(ctx context.Context, teamUID string) (string, bool, error) {
	if r.reader == nil {
		return "", false, errors.New("party advancer: teamCoordinatorRoster: nil reader")
	}
	if strings.TrimSpace(teamUID) == "" {
		return "", false, errors.New("party advancer: teamCoordinatorRoster: empty team uid")
	}

	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return "", false, fmt.Errorf("party advancer: list teams: %w", err)
	}
	var team *ksquadv1.Team
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			team = &teams.Items[i]
			break
		}
	}
	if team == nil {
		return "", false, fmt.Errorf("party advancer: team uid %s resolves to no Team CR", teamUID)
	}

	// Index the Coordinator-role names in the Team's namespace (ISI-5283 — the Team-admission webhook
	// enforces ≤1 Coordinator role per Team, so at most one agent carries it). A dangling roleRef simply
	// resolves to non-coordinator; the Role's own guard owns rejection, this never fabricates a badge.
	var roles ksquadv1.RoleList
	if err := r.reader.List(ctx, &roles, client.InNamespace(team.Namespace)); err != nil {
		return "", false, fmt.Errorf("party advancer: list roles in %q: %w", team.Namespace, err)
	}
	coordinatorRole := make(map[string]bool, len(roles.Items))
	for i := range roles.Items {
		if roles.Items[i].Spec.Coordinator {
			coordinatorRole[roles.Items[i].Name] = true
		}
	}

	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, client.InNamespace(team.Namespace)); err != nil {
		return "", false, fmt.Errorf("party advancer: list agents in %q: %w", team.Namespace, err)
	}
	agentRole := make(map[string]string, len(agents.Items))
	for i := range agents.Items {
		agentRole[agents.Items[i].Name] = agents.Items[i].Spec.RoleRef.Name
	}

	// Walk the Team's agents in roster order; the first whose Role is the Coordinator role is the one the
	// mint-gate will recognise as the facilitator author.
	for _, ref := range team.Spec.Agents {
		if ref.Name == "" {
			continue
		}
		if role := agentRole[ref.Name]; role != "" && coordinatorRole[role] {
			return ref.Name, true, nil
		}
	}

	slog.WarnContext(ctx, "party advancer: team has no Coordinator-role agent — not minting facilitator (fail safe, no broadcast)",
		"teamUID", teamUID, "namespace", team.Namespace, "agents", len(team.Spec.Agents))
	return "", false, nil
}
