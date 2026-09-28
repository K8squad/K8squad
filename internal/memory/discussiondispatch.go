package memory

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// discussiondispatch.go — dispatch-on-mention parity for the cmd/memory `discussion_post` MCP tool
// (ISI-5125, follow-up to ISI-5116).
//
// THE GAP. Dispatch-on-mention's trigger (parse @-mentions → mint a Run) and ISI-5116's reply-hop
// auto-stamp both live on the apiserver REST discussion Handler. The discussion_post MCP tool runs in a
// SEPARATE process (cmd/memory) and posts via discussion.Store.PostMessage DIRECTLY — so a message posted
// through that tool fired NEITHER the mention-dispatch trigger NOR the loop-guard hop stamp. If a dispatched
// Run replies via the MCP tool rather than the REST endpoint, agent→agent chaining silently would not
// trigger — the same "looks wired, isn't live" class as ISI-4919.
//
// THE FIX. DiscussionDispatch bundles the two seams (the run-minting MentionDispatcher + the reply
// ReplyHopResolver) and a roster source, and drives them through the SAME shared free functions the REST
// handler delegates to (discussion.StampReplyHopFrom / discussion.DispatchMentionsFrom, ISI-5125). The two
// edges therefore cannot drift: the guardrail application, the hop stamp, and the MentionDispatch wiring
// have exactly one implementation.
//
// OPT-OUT DEGRADE (documented, per the issue). resolveMentionTargets needs a roster with a presence bucket
// for the opt-out guardrail (a paused/blocked agent is not auto-dispatched). cmd/memory has the Team-CR
// informer cache (coord.TeamAgentResolver) which yields agent NAMES but no presence. So on the tool path
// Status is empty and opt-out DEGRADES to "never opts out" — the OTHER three guardrails (loop cap ≤2,
// de-dupe, rate 5/msg) still bound the blast radius, and the REST reply path (the transport ADR-0024b §5
// instructs dispatched runs to use) enforces opt-out fully. Reaching the full org presence projection from
// cmd/memory would couple it to the apiserver read model; the parity that matters for agent↔agent chaining
// (the @-mention resolves and dispatches, the hop is bounded) does not need presence.

// RosterResolver resolves the dispatchable roster (agent names) for the caller's Team on the MCP tool
// dispatch path. It is the narrow slice of the room roster resolveMentionTargets matches @-mentions
// against. cmd/memory backs it with the SAME Team-CR informer cache that authorizes work_item_assign.
type RosterResolver interface {
	// RosterForTeam lists the agents of the Team whose CR uid is teamID — the MCP caller's own tenancy
	// (an agent/run is never admin, so it always resolves its own squad, the room's team). Status is
	// best-effort: an empty Status degrades the opt-out guardrail (see file header).
	RosterForTeam(ctx context.Context, teamID uuid.UUID) ([]discussion.TeamAgent, error)
}

// coordRosterResolver adapts coord.TeamAgentResolver (the Team-CR informer cache, ISI-4743) to
// RosterResolver: it lists the caller Team's agent names for the @-mention match. Presence is unavailable
// from that cache, so Status is left empty (opt-out degrades, documented above).
type coordRosterResolver struct {
	resolver coord.TeamAgentResolver
}

// NewCoordRosterResolver builds a RosterResolver over the Team-CR informer cache. A nil resolver yields a
// nil RosterResolver, so the caller leaves dispatch-on-mention off on the tool path (parity with the
// apiserver's nil-roster degrade) rather than dispatching against an empty world.
func NewCoordRosterResolver(resolver coord.TeamAgentResolver) RosterResolver {
	if resolver == nil {
		return nil
	}
	return coordRosterResolver{resolver: resolver}
}

func (r coordRosterResolver) RosterForTeam(ctx context.Context, teamID uuid.UUID) ([]discussion.TeamAgent, error) {
	names, err := r.resolver.TeamAgents(ctx, teamID.String())
	if err != nil {
		return nil, err
	}
	out := make([]discussion.TeamAgent, 0, len(names))
	for _, n := range names {
		out = append(out, discussion.TeamAgent{Name: n}) // Status "" ⇒ opt-out degraded on the tool path
	}
	return out, nil
}

// DiscussionDispatch is the optional dispatch-on-mention wiring for the discussion_post tool (ISI-5125):
// symmetric with the REST handler's trigger + hop-stamp so a message posted via the MCP tool ALSO fires
// mention dispatch and carries the loop-guard hop. It is nil-safe end to end: a nil *DiscussionDispatch (a
// DB-less / cluster-less deployment, or one where the run-minting seams could not be built) leaves the tool
// posting exactly as before — no dispatch, no stamp.
type DiscussionDispatch struct {
	dispatcher  discussion.MentionDispatcher
	hopResolver discussion.ReplyHopResolver
	roster      RosterResolver
}

// NewDiscussionDispatch bundles the run-minting dispatcher, the reply-hop resolver, and the roster source.
// Any component may be nil and degrades gracefully: a nil dispatcher/roster disables the trigger, a nil
// hopResolver disables the auto-stamp — the post still commits. Returns nil when there is nothing to wire
// (no dispatcher AND no hopResolver), so the tool surface treats it as "dispatch off".
func NewDiscussionDispatch(dispatcher discussion.MentionDispatcher, hopResolver discussion.ReplyHopResolver, roster RosterResolver) *DiscussionDispatch {
	if dispatcher == nil && hopResolver == nil {
		return nil
	}
	return &DiscussionDispatch{dispatcher: dispatcher, hopResolver: hopResolver, roster: roster}
}

// stampReplyHop returns the payload a tool-posted reply should be stored with, auto-stamping the loop-guard
// hop from the Run's identity (auth.RunID) via the shared discussion.StampReplyHopFrom. The MCP tool
// carries no inbound payload, so the base is nil; a non-agent/non-run post or an unwired resolver returns
// nil unchanged. This is the SAME stamp the REST handler applies pre-write (ISI-5116).
func (d *DiscussionDispatch) stampReplyHop(ctx context.Context, auth discussion.AuthorContext) *json.RawMessage {
	if d == nil {
		return nil
	}
	return discussion.StampReplyHopFrom(ctx, d.hopResolver, auth, nil)
}

// dispatchMentions is the tool's post-commit trigger: it resolves the caller Team's roster and delegates to
// the shared discussion.DispatchMentionsFrom, so a @-mention in a tool-posted message dispatches exactly
// like the REST path. Best-effort — the message is already durable, so a roster or dispatch failure never
// fails the tool call. No-ops when dispatch or the roster is unwired.
func (d *DiscussionDispatch) dispatchMentions(ctx context.Context, projectID string, auth discussion.AuthorContext, msg *discussion.Message) {
	if d == nil || d.dispatcher == nil || d.roster == nil || msg == nil {
		return
	}
	roster, err := d.roster.RosterForTeam(ctx, auth.TeamID)
	if err != nil {
		return // the roster is a projection, not a fence — a read failure degrades to no dispatch
	}
	discussion.DispatchMentionsFrom(ctx, d.dispatcher, roster, projectID, auth, msg)
}
