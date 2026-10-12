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

// CoordinatorCapability is the server-authenticated provenance marker (ISI-5688) a discussion run's token
// carries when its dispatched agent holds the Team Coordinator role. It mirrors
// pkg/capability.CapabilityCoordinator — kept as a local const so this package does not import the operator
// capability package, matching CoordinatorProposeCapability's local mirror of its slug.
const CoordinatorCapability = "coordinator"

// SessionHasCoordinatorCapability reports whether the server-authenticated MCP session carries the
// CoordinatorCapability marker — the un-spoofable "this post is from the Team Coordinator" signal the party
// dispatch hook needs on the MCP edge (the name-only tool roster has no Coordinator flag). On the token
// (sandbox) path the capability set is the VERIFIED token claims; on the header (BFF) path it is the
// control-plane-stamped X-Agent-Capabilities. Either way the tool caller cannot forge it.
func SessionHasCoordinatorCapability(capabilities []string) bool {
	for _, c := range capabilities {
		if c == CoordinatorCapability {
			return true
		}
	}
	return false
}

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
	refResolver discussion.TicketRefResolver  // ISI-5166: ticket-reference resolution seam; nil ⇒ references dropped
	partyStore  discussion.PartyDispatchStore // ISI-5688: party-session seam so the MCP edge fires the party-facilitator roster stamp; nil ⇒ party path off (unchanged)
}

// WithPartyStore wires the party-session seam (ISI-5688) onto the discussion_post dispatch bundle so a
// facilitator run posting its round directive through the MCP tool stamps the party round roster — parity
// with the apiserver REST Handler, whose dispatchMentions already runs DispatchPartyMentionsFrom. Without
// it the MCP edge only ran the plain DispatchMentionsFrom and the party round stalled (zero voices). A nil
// store leaves the party path off (the tool posts exactly as before). Returns the receiver for chaining.
func (d *DiscussionDispatch) WithPartyStore(store discussion.PartyDispatchStore) *DiscussionDispatch {
	if d == nil {
		return d
	}
	d.partyStore = store
	return d
}

// NewDiscussionDispatch bundles the run-minting dispatcher, the reply-hop resolver, the roster source, and
// the ticket-reference resolver. Any component may be nil and degrades gracefully: a nil dispatcher/roster
// disables the trigger, a nil hopResolver disables the auto-stamp, a nil refResolver drops ticket references
// — the post still commits. Returns nil when there is nothing to wire (no dispatcher AND no hopResolver AND
// no refResolver), so the tool surface treats it as "dispatch off".
func NewDiscussionDispatch(dispatcher discussion.MentionDispatcher, hopResolver discussion.ReplyHopResolver, roster RosterResolver, refResolver discussion.TicketRefResolver) *DiscussionDispatch {
	if dispatcher == nil && hopResolver == nil && refResolver == nil {
		return nil
	}
	return &DiscussionDispatch{dispatcher: dispatcher, hopResolver: hopResolver, roster: roster, refResolver: refResolver}
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

// stampTicketRefs (ISI-5166) is the tool's pre-write ticket-reference hook: it resolves the request's
// candidate references against the message's project and merges the surviving links into the payload under
// `references`, driving the SAME shared discussion.StampTicketRefsFrom the REST handler delegates to — so a
// ticket referenced through the MCP tool persists into Message.Payload identically to the REST path, and an
// unknown/out-of-project ref is dropped identically. Best-effort and nil-safe: a nil bundle, a nil
// refResolver, no candidates, a resolver error, or an all-out-of-project set all pass the payload through
// unchanged. A reference is a LINK, never a dispatch — this hook emits no MentionDispatch.
func (d *DiscussionDispatch) stampTicketRefs(ctx context.Context, projectID string, auth discussion.AuthorContext, refs []discussion.TicketRef, payload *json.RawMessage) *json.RawMessage {
	if d == nil {
		return payload
	}
	return discussion.StampTicketRefsFrom(ctx, d.refResolver, projectID, auth, refs, payload)
}

// dispatchMentions is the tool's post-commit trigger: it resolves the caller Team's roster and delegates to
// the shared discussion.DispatchMentionsFrom, so a @-mention in a tool-posted message dispatches exactly
// like the REST path. Best-effort — the message is already durable, so a roster or dispatch failure never
// fails the tool call. No-ops when dispatch or the roster is unwired.
// authorIsCoordinator is the server-authenticated "the posting agent holds the Team Coordinator role"
// signal (ISI-5688): the MCP tool caller derives it from the run token's CapabilityCoordinator marker,
// because the name-only tool roster carries no Coordinator flag the party hook could read. It is false on
// every non-token / non-coordinator post, which keeps the party path inert exactly as before.
func (d *DiscussionDispatch) dispatchMentions(ctx context.Context, projectID string, auth discussion.AuthorContext, msg *discussion.Message, authorIsCoordinator bool) {
	if d == nil || d.dispatcher == nil || d.roster == nil || msg == nil {
		return
	}
	roster, err := d.roster.RosterForTeam(ctx, auth.TeamID)
	if err != nil {
		return // the roster is a projection, not a fence — a read failure degrades to no dispatch
	}
	// ISI-5688: fire the party-facilitator roster stamp FIRST (parity with the REST Handler.dispatchMentions
	// order), so a coordinator's round directive posted through discussion_post records RoundVoices and the
	// advancer can sequence the voices. On a non-party / non-coordinator post this returns false and we fall
	// through to the unchanged normal dispatch. (The REST-only bare-party facilitator ROUTE — ADR-0031 Ruling
	// B — never applies here: it is human-authored only, and an MCP post is always agent-authored.)
	if d.partyStore != nil && discussion.DispatchPartyMentionsFrom(ctx, d.dispatcher, d.partyStore, projectID, auth, msg, roster, authorIsCoordinator) {
		return
	}
	discussion.DispatchMentionsFrom(ctx, d.dispatcher, roster, projectID, auth, msg)
}
