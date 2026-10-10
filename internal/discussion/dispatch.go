package discussion

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ============================================================================
// Dispatch-on-mention (ISI-5108, plan ISI-4919) — the room's ONLY automatic
// path from a posted message to an agent Run.
// ============================================================================
//
// Reported by Henrik (ISI-4919): "no chat is actually happening" — @-mentioning an agent produced no
// response. The room was deliberately coordination-free (PostMessage parsed no @ tokens and enqueued
// no dispatch); the sole room→agent path was the human-gated Proposal card. So an @-mention was inert
// text.
//
// This file adds the TRIGGER half: after postMessage commits a row, the handler parses @name tokens
// against the resolved roster (ISI-5107) and, for each matched agent, emits a MentionDispatch through
// the MentionDispatcher seam. The seam is what apiserver wires to real run-minting (the same
// RequestDispatch fan-out proposalconfirm uses) so THIS layer opens no new run pathway — it only
// decides WHO to dispatch, applying the four required guardrails:
//
//   - Loop guard   — an agent-authored message may not re-trigger dispatch past a bounded hop depth
//     (maxMentionHopDepth). A human post is hop 0 (a fresh turn); each agent→agent hop increments,
//     tracked in-band via the message payload (_dispatch.hopDepth) per the plan. Without this two
//     agents can @ each other into an infinite paid loop.
//   - De-dupe      — one dispatch per (messageId, agent): candidate names are de-duplicated within a
//     post, and the seam is handed (MessageID, AgentName) so the implementer can key idempotency.
//   - Rate/cost    — at most maxMentionDispatchPerMessage auto-dispatches per posted message; excess
//     mentions are dropped (surfaced via the returned count so the caller can log the cap).
//   - Opt-out      — a paused/blocked agent is never auto-dispatched.
//
// Audience semantics: `direct:<agent>` dispatches ONLY that agent; a `party` post with @-mentions
// dispatches each @-mentioned agent. A bare `party` post with no @-mention BROADCASTS to the whole
// room roster when it is a HUMAN turn (ISI-5265 — "party" means "talk to the whole room", and a human
// at hop 0 is the one turn allowed to wake the squad); an AGENT-authored bare party post still
// dispatches nobody (loop-safety: broadcast is a hop-0 human privilege, never an agent's, or two
// agents party-posting would N²-loop the squad).

// MentionDispatch is one resolved auto-dispatch intent: a specific agent to run with a discussion
// thread as context, carrying the loop-guard hop depth and the provenance of the message that
// triggered it. apiserver turns this into a real Run through the existing dispatch seam; the run reads
// the thread and posts its reply back via the authenticated POST .../messages path (no new write
// path). It is deliberately transport-free — the decision layer never touches coord or a Run.
type MentionDispatch struct {
	ProjectID string    // "namespace/name" slug (ISI-3982) — the room key
	ThreadID  uuid.UUID // the thread the dispatched agent must read + reply into
	MessageID uuid.UUID // the triggering message (idempotency key half: one dispatch per (messageId, agent))
	TeamID    uuid.UUID // the room's Team scope — the tenancy the minted Run lands in
	AgentName string    // the mentioned agent (the @-token, matched against the roster)

	// HopDepth is the hop this dispatch runs AT (a human turn dispatches at hop 1). The dispatched
	// run must stamp its post-back with this depth so a further @-mention is bounded — see
	// StampDispatchHop / DispatchHopOf.
	HopDepth int

	// Provenance of the TRIGGERING message (server-stamped, never body-supplied): the principal
	// always present; TriggeredByAgentID set ⇒ an agent authored the trigger (the loop-guard signal).
	TriggeredByPrincipal string
	TriggeredByAgentID   *string

	// Orchestrate marks this as a MULTI-MENTION coordinator dispatch (ISI-5283 / ISI-5267 WS-2):
	// instead of one Run per mentioned agent, a post that resolves 2+ mentions dispatches the Team's
	// Coordinator role ONCE (AgentName is the coordinator) with a structured directive to structure
	// the work involving the mentioned agents. The coordinator is the LIVE ISI-5220 orchestrator
	// (work_item_create + work_item_assign). When false this is an ordinary single-agent reply
	// dispatch and the two fields below are empty.
	Orchestrate bool
	// OrchestratedAgents is the resolved (roster-matched) set of mentioned agents the coordinator is
	// asked to structure the work for, in first-seen order. Set only when Orchestrate is true.
	OrchestratedAgents []string
	// RequestBody is the verbatim triggering comment body — the "the request is: «…»" half of the
	// coordinator directive. Set only when Orchestrate is true.
	RequestBody string

	// Party, when non-nil, carries the cross-talk context + persona prompt for a party-mode voice
	// dispatch (ISI-5586 WS-C, ADR-0027 §3.2). The facilitator (WS-D) assembles it per voice per round
	// — persona blurb + "What Others Said This Round" + rolling <400-word summary + disagree/pass
	// guidelines — and the run-minting half renders it into the dispatched work-item body. It is nil for
	// every ordinary single-agent reply and every orchestration dispatch, so those bodies are untouched.
	Party *PartyContext
}

// MentionDispatcher is the seam the §13 apiserver supplies: it turns a resolved MentionDispatch into a
// real agent Run (via the same RequestDispatch fan-out proposalconfirm.go rides — this layer invents
// no run pathway). It MUST be idempotent on (MessageID, AgentName) so an at-least-once caller cannot
// double-run. A nil dispatcher (DB-less dev runs, or before the run-minting wiring lands) leaves the
// room coordination-free exactly as before — postMessage still commits, it just dispatches nobody.
type MentionDispatcher interface {
	DispatchMention(ctx context.Context, d MentionDispatch) error
}

// SetMentionDispatcher wires the dispatch seam onto a handler built via NewHandler/NewHandlerWithDeps.
// It is a post-construction setter (not a constructor arg) so the existing constructors — and every
// test that rides them — stay source-compatible. A handler with no dispatcher (the default) parses no
// mentions and dispatches nobody.
func (h *Handler) SetMentionDispatcher(d MentionDispatcher) { h.dispatcher = d }

// ReplyHopResolver is the ISI-5116 reply-plumbing seam: given the (server-stamped) id of the Run a
// message was posted from, it returns the loop-guard hop the Run was dispatched to reply at, if that
// Run is a dispatch-on-mention thread-run. This is what lets an agent→agent reply carry an HONEST hop
// without trusting the agent to echo the number: postMessage stamps it from the Run's identity alone,
// via the dispatch ledger (apiserver supplies the implementation). `ok=false` for any Run that is not a
// thread-run (a normal post stays hop 0). An error degrades to no stamp — best-effort, the reply is
// already durable and valid; the other three guardrails (de-dupe, rate cap, opt-out) still bound the
// blast radius. A nil resolver (DB-less dev runs, or before this wiring lands) leaves replies unstamped
// exactly as before, so a plain post remains hop 0.
type ReplyHopResolver interface {
	HopForDispatchedRun(ctx context.Context, runID string) (hop int, ok bool, err error)
}

// SetReplyHopResolver wires the reply-hop seam onto the handler (post-construction, same rationale as
// SetMentionDispatcher). Without it, agent replies are not auto-stamped.
func (h *Handler) SetReplyHopResolver(r ReplyHopResolver) { h.hopResolver = r }

const (
	// maxMentionHopDepth bounds agent→agent chaining: a human post is hop 0, so the run it dispatches
	// is hop 1; that run's @-mention dispatches hop 2; a hop-2 run's @-mention would be hop 3 and is
	// refused. This is the plan's "max 2 hops per human turn" — enough for a PM→implementer→reviewer
	// exchange, closed before an unbounded paid loop.
	maxMentionHopDepth = 2

	// maxMentionDispatchPerMessage caps the fan-out of a single post (rate/cost guardrail): a message
	// that @-mentions the whole squad dispatches at most this many agents. Excess mentions are dropped.
	maxMentionDispatchPerMessage = 5

	// maxBroadcastDispatchPerMessage caps a whole-room broadcast (ISI-5265): a HUMAN party post with no
	// @-mention wakes every eligible roster agent, and the 5-mention cap is too small for a full squad.
	// This is sized for a realistic squad room and stays bounded — a broadcast that would exceed it
	// dispatches the first this-many and counts the rest via `dropped` (a non-silent truncation the
	// caller logs), never fanning out unbounded. Only the human hop-0 broadcast path uses this cap;
	// an explicit @-mention party post still rides the smaller maxMentionDispatchPerMessage.
	maxBroadcastDispatchPerMessage = 25

	// dispatchPayloadKey is the message-payload field carrying the in-band loop-guard hop counter
	// (plan: "a per-thread hop counter in payload"). Human posts omit it (hop 0); a dispatched run
	// stamps it on its reply so the NEXT mention parse sees the accumulated depth.
	dispatchPayloadKey = "_dispatch"

	// referencesPayloadKey is the message-payload field carrying the resolved ticket references
	// (ISI-5165) — the LINK-not-dispatch metadata. It shares the payload object with dispatchPayloadKey
	// via the same JSON-merge pattern, so a message can carry both a loop-guard hop and ticket links.
	referencesPayloadKey = "references"
)

// mentionTokenRe matches an @-mention token in a message body: '@' then a name that starts with an
// alphanumeric and may carry the interior punctuation agent names use (hyphen/underscore/dot), e.g.
// "@Robo-Coder", "@john". The captured name is resolved case-insensitively against the roster, so an
// unknown token simply matches nobody (inert, as before). Emails/times ("a@b", "10:@x") don't start a
// token at a word boundary the way a mention does; a trailing '.' (sentence punctuation) is trimmed
// by the roster match, never smuggled into the name.
var mentionTokenRe = regexp.MustCompile(`(^|[^\w@/])@([A-Za-z0-9][A-Za-z0-9_.-]*)`)

// parseMentions extracts the distinct @-mention names from a message body, preserving first-seen
// order and de-duplicating case-insensitively (one dispatch per agent — the de-dupe guardrail starts
// here). A trailing '.' is stripped so "@john." resolves to "john"; the roster match is the final
// authority on what is a real agent.
func parseMentions(body string) []string {
	matches := mentionTokenRe.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimRight(m[2], ".")
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// StampDispatchHop returns a payload that carries the loop-guard hop depth, merging into any payload
// the caller already has. A dispatched Run calls this (via apiserver) when it posts its reply so the
// next mention parse in the thread sees the accumulated depth. Exported so the run-context plumbing
// (the seam implementer) can stamp the reply without re-deriving the payload shape.
func StampDispatchHop(existing *json.RawMessage, hopDepth int) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if existing != nil && len(*existing) > 0 {
		_ = json.Unmarshal(*existing, &obj) // a non-object payload is replaced rather than corrupted
	}
	meta, _ := json.Marshal(map[string]int{"hopDepth": hopDepth})
	obj[dispatchPayloadKey] = meta
	out, _ := json.Marshal(obj)
	return out
}

// DispatchHopOf reads the in-band hop counter from a message payload. Absent/malformed ⇒ 0, so a
// human post (no _dispatch field) is always a fresh turn.
func DispatchHopOf(payload *json.RawMessage) int {
	if payload == nil || len(*payload) == 0 {
		return 0
	}
	var wrapper struct {
		Dispatch struct {
			HopDepth int `json:"hopDepth"`
		} `json:"_dispatch"`
	}
	if err := json.Unmarshal(*payload, &wrapper); err != nil {
		return 0
	}
	if wrapper.Dispatch.HopDepth < 0 {
		return 0
	}
	return wrapper.Dispatch.HopDepth
}

// partyBroadcastRequested reports whether a message explicitly opted into the DEMOTED simultaneous
// broadcast affordance via the party-mode payload field {"_party":{"broadcast":true}} (ADR-0031 Ruling B
// §3.2). This is the console's "ask everyone in parallel" opt-in — it merges alongside _dispatch /
// references in the same JSON-object payload shape. Absent/malformed ⇒ false, so a plain human party post
// defaults to the sequenced-facilitator route: the whole point of Ruling B is that the legacy one-shot
// parallel fan-out is retired as the DEFAULT, kept only behind this explicit, rate-limited affordance.
func partyBroadcastRequested(payload *json.RawMessage) bool {
	if payload == nil || len(*payload) == 0 {
		return false
	}
	var wrapper struct {
		Party struct {
			Broadcast bool `json:"broadcast"`
		} `json:"_party"`
	}
	if err := json.Unmarshal(*payload, &wrapper); err != nil {
		return false
	}
	return wrapper.Party.Broadcast
}

// ============================================================================
// Ticket references (ISI-5165, plan ISI-5134 S1) — a LINK, NOT a dispatch
// ============================================================================
//
// A discussion message may carry structured references to work items (tickets). A reference is a LINK:
// it is persisted as durable metadata in the message payload and rendered as a chip, but it MUST NOT
// dispatch anything and pulls NO context into any agent run (Q2 default = NO, confirmed by the plan).
// It therefore bypasses resolveMentionTargets / DispatchMentionsFrom entirely — ISI-5108 dispatch stays
// agent-only, driven solely by @-mention tokens in the body.
//
// k8squad work items are UUID-keyed (no human ISI-#### column), so the MVP is picker-by-title carrying
// the work-item UUID plus its display title (Q1 Option A). Typed "#ISI-1234" literal refs are OUT OF
// SCOPE (they would need a ref-column migration). Each candidate UUID is validated against the message's
// project via the TicketRefResolver seam; unknown or out-of-project refs are dropped, never persisted.

// TicketRef is one ticket reference — a link from a message to a work item. WorkItemID is the coord
// work-item UUID (the picker's carried key); Title is its display title for chip rendering. Persisted
// into Message.Payload under referencesPayloadKey; it is never a dispatch target.
type TicketRef struct {
	WorkItemID string `json:"workItemId"`
	Title      string `json:"title,omitempty"`
}

// StampReferences merges validated ticket references into a message payload under the `references`
// key, using the same JSON-merge pattern as StampDispatchHop so a payload can carry both a loop-guard
// hop (_dispatch) and ticket links (references) without either clobbering the other. An empty ref set
// returns the payload unchanged (no `references` key is added), so a plain message stays link-free.
func StampReferences(existing *json.RawMessage, refs []TicketRef) *json.RawMessage {
	if len(refs) == 0 {
		return existing
	}
	obj := map[string]json.RawMessage{}
	if existing != nil && len(*existing) > 0 {
		_ = json.Unmarshal(*existing, &obj) // a non-object payload is replaced rather than corrupted
	}
	encoded, _ := json.Marshal(refs)
	obj[referencesPayloadKey] = encoded
	out, _ := json.Marshal(obj)
	raw := json.RawMessage(out)
	return &raw
}

// ReferencesOf reads the ticket references from a message payload. Absent/malformed ⇒ nil, so a plain
// message (no `references` field) carries no links.
func ReferencesOf(payload *json.RawMessage) []TicketRef {
	if payload == nil || len(*payload) == 0 {
		return nil
	}
	var wrapper struct {
		References []TicketRef `json:"references"`
	}
	if err := json.Unmarshal(*payload, &wrapper); err != nil {
		return nil
	}
	return wrapper.References
}

// normalizeTicketRefs is the pure pre-resolution pass (no I/O): it de-duplicates candidate references
// by work-item id (first-seen wins, order preserved) and drops any with a blank or non-UUID id BEFORE
// the resolver's project-membership check — so a malformed or empty token never reaches coord and the
// UUID-keyed MVP contract is enforced client-independently. The surviving title is carried through
// verbatim; the resolver is free to canonicalize it against the authoritative source.
func normalizeTicketRefs(refs []TicketRef) []TicketRef {
	seen := make(map[string]bool, len(refs))
	out := make([]TicketRef, 0, len(refs))
	for _, r := range refs {
		id := strings.TrimSpace(r.WorkItemID)
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue // work items are UUID-keyed (Q1 Option A); a non-UUID token is not a valid ref
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, TicketRef{WorkItemID: id, Title: r.Title})
	}
	return out
}

// TicketRefResolver is the ISI-5165 resolution seam: given the room's projectID, the caller's Team
// scope, and candidate references, it returns the subset that EXIST and belong to that project —
// the LINK-not-dispatch guardrail's resolution half. It reuses the coord project-narrowing ride
// (searchWorkItems / ReadWorkItemThread) and may canonicalize each returned Title from the
// authoritative source; unknown or out-of-project refs are dropped (never persisted). apiserver
// supplies the implementation. A nil resolver (DB-less dev runs, or before the wiring lands) resolves
// nothing, so no references are persisted and a post stays link-free exactly as before.
type TicketRefResolver interface {
	ResolveTicketRefs(ctx context.Context, projectID string, teamID uuid.UUID, refs []TicketRef) ([]TicketRef, error)
}

// SetTicketRefResolver wires the reference-resolution seam onto the handler (post-construction, same
// rationale as SetMentionDispatcher / SetReplyHopResolver). Without it, ticket references are dropped.
func (h *Handler) SetTicketRefResolver(r TicketRefResolver) { h.refResolver = r }

// StampTicketRefsFrom is the shared pre-write ticket-reference hook BOTH room write edges call: the
// apiserver REST Handler.postMessage (ISI-5165) and the cmd/memory discussion_post MCP tool (ISI-5166),
// which runs in a separate process and posts via discussion.Store directly. It normalizes the request's
// candidate references, resolves them against the message's project through the resolver seam (dropping
// unknown/out-of-project refs), and merges only the surviving links into the payload under `references`.
// Hoisting it here — with the resolver seam passed in — is what keeps the two edges from drifting on how
// a reference is normalized, resolved, and persisted (the same rationale as StampReplyHopFrom /
// DispatchMentionsFrom, ISI-5125). It is best-effort by construction — a nil resolver, no candidates, a
// resolver error, or an empty result all pass the payload through unchanged. A reference is durable link
// metadata, NEVER a write fence (the message is the durable artifact) and NEVER a dispatch (this hook
// emits no MentionDispatch).
func StampTicketRefsFrom(ctx context.Context, resolver TicketRefResolver, projectID string, auth AuthorContext, refs []TicketRef, payload *json.RawMessage) *json.RawMessage {
	if resolver == nil {
		return payload
	}
	candidates := normalizeTicketRefs(refs)
	if len(candidates) == 0 {
		return payload
	}
	resolved, err := resolver.ResolveTicketRefs(ctx, projectID, auth.TeamID, candidates)
	if err != nil || len(resolved) == 0 {
		return payload // best-effort: an unresolvable set degrades to a link-free (but durable) message
	}
	return StampReferences(payload, resolved)
}

// stampTicketRefs (ISI-5165) is the REST handler's pre-write hook; it delegates to the shared
// StampTicketRefsFrom (ISI-5166) so the REST and MCP-tool edges resolve and persist references
// identically.
func (h *Handler) stampTicketRefs(ctx context.Context, projectID string, auth AuthorContext, refs []TicketRef, payload *json.RawMessage) *json.RawMessage {
	return StampTicketRefsFrom(ctx, h.refResolver, projectID, auth, refs, payload)
}

// dispatchableStatus reports whether an agent in the given presence bucket may be auto-dispatched: the
// opt-out guardrail. A paused/blocked/disabled agent is skipped (case-insensitive), so an operator
// pause reliably suppresses auto-runs; any other status (working/idle/offline/…) is dispatchable —
// an offline agent still has intent recorded and Intake wakes it, exactly like a board dispatch.
func dispatchableStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "paused", "blocked", "disabled":
		return false
	default:
		return true
	}
}

// resolveMentionTargets is the pure decision core (no I/O): given a committed message, the roster, and
// the trigger provenance, it returns the agents to auto-dispatch and the hop depth they run at. It
// applies every guardrail EXCEPT idempotency (which is the seam's job) so it is exhaustively unit
// testable without a database. `dropped` reports how many candidates the rate cap discarded, so the
// caller can log a non-silent truncation.
func resolveMentionTargets(msg *Message, roster []TeamAgent) (targets []MentionDispatch, dropped int, fallback bool) {
	// (1) Loop guard: hop this message sits at (human post ⇒ 0). The run a mention here dispatches
	// runs at hop+1; if that would exceed the cap, an agent-authored message dispatches nobody — the
	// paid-loop backstop. A human post always passes (0+1 ≤ cap).
	msgHop := 0
	if msg.AuthorAgentID != nil {
		msgHop = DispatchHopOf(msg.Payload)
	}
	nextHop := msgHop + 1
	if nextHop > maxMentionHopDepth {
		return nil, 0, false
	}

	// (2) Audience routing: direct:<agent> targets exactly that agent (ignoring body @-tokens); a
	// party post with @-mentions targets each mentioned agent. A party post with NO @-mention
	// BROADCASTS to the whole room roster (ISI-5265) — but ONLY when the trigger is a HUMAN turn
	// (msg.AuthorAgentID == nil). "party" means "talk to the whole room", and a human at hop 0 is the
	// one turn allowed to wake the squad. This intentionally reverses the ISI-5108 guardrail, but only
	// for human-authored posts: an AGENT-authored party post with no @-mention still dispatches nobody
	// (loop-safety — broadcast is a hop-0 human privilege, never an agent's; otherwise two agents
	// party-posting would N²-loop the whole squad. Agent posts continue to dispatch ONLY via explicit
	// @-mention, bounded by the hop guard above).
	var candidates []string
	broadcast := false
	if target, ok := strings.CutPrefix(msg.Audience, "direct:"); ok && target != "" {
		candidates = []string{target}
	} else {
		candidates = parseMentions(msg.Body)
		if len(candidates) == 0 {
			if msg.AuthorAgentID != nil {
				return nil, 0, false // agent party + no @-mention: dispatch nobody (loop-safety)
			}
			// human party + no @-mention: broadcast to every roster agent (guardrails below still
			// apply — opt-out, self-exclusion, de-dupe, and the broadcast fan-out cap).
			broadcast = true
			candidates = make([]string, 0, len(roster))
			for _, a := range roster {
				if a.Name != "" {
					candidates = append(candidates, a.Name)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, 0, false
	}

	// (3) Resolve each candidate against the roster (case-insensitive), applying the opt-out and
	// self-mention guardrails and de-duplicating. The full resolvable set is collected FIRST — before
	// the fan-out cap — so the multi-mention routing decision (ISI-5283) keys on the TRUE count of
	// resolvable agents, not the post-cap count.
	byName := make(map[string]TeamAgent, len(roster))
	for _, a := range roster {
		if a.Name != "" {
			byName[strings.ToLower(a.Name)] = a
		}
	}
	// The trigger's own agent identity, so an agent that @-mentions itself does not self-dispatch.
	selfAgent := ""
	if msg.AuthorAgentID != nil {
		selfAgent = strings.ToLower(*msg.AuthorAgentID)
	}

	resolved := make([]TeamAgent, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		agent, ok := byName[key]
		if !ok {
			continue // unknown token — inert, matches nobody
		}
		if !dispatchableStatus(agent.Status) {
			continue // opt-out: paused/blocked agents are not auto-dispatched
		}
		if key == selfAgent {
			continue // an agent never auto-dispatches itself
		}
		seen[key] = true
		resolved = append(resolved, agent)
	}
	if len(resolved) == 0 {
		return nil, 0, false
	}

	// (4) Multi-mention routing (ISI-5283 / ISI-5267 WS-2, decision D2): an EXPLICIT @-mention post
	// that resolves 2+ agents does NOT fan out one Run per mentioned agent. Instead it dispatches the
	// Team's Coordinator role ONCE, carrying a structured directive to structure the work involving the
	// mentioned agents — the coordinator is the LIVE ISI-5220 orchestrator (work_item_create +
	// work_item_assign). The threshold is the count of RESOLVABLE mentions (D2), so "@a @ghost" with
	// only @a on the roster stays a single direct dispatch. A whole-room broadcast (human party, no @)
	// is NOT a multi-mention post and keeps its fan-out.
	if !broadcast && len(resolved) >= 2 {
		coord, ok := coordinatorOf(roster)
		// A coordinator that authored the triggering post would self-dispatch; treat that as "no
		// coordinator available" so it falls back to the picker rather than looping on itself.
		if ok && strings.ToLower(coord.Name) == selfAgent {
			ok = false
		}
		if !ok {
			// D3 fallback: the Team has NO (dispatchable) Coordinator role. Do NOT silently fan out to
			// every mentioned agent — signal the caller to surface a lightweight picker/prompt instead.
			return nil, 0, true
		}
		mentioned := make([]string, 0, len(resolved))
		for _, a := range resolved {
			mentioned = append(mentioned, a.Name) // canonical roster casing, first-seen order
		}
		return []MentionDispatch{{
			AgentName:          coord.Name, // canonical roster casing
			HopDepth:           nextHop,
			Orchestrate:        true,
			OrchestratedAgents: mentioned,
			RequestBody:        msg.Body,
		}}, 0, false
	}

	// (5) Single-agent / broadcast path: one dispatch per resolved agent, bounded by the fan-out cap.
	// A whole-room broadcast rides the larger squad-sized cap; a single @-mention (or a direct: post)
	// stays on the smaller per-message cap. Either way the fan-out is bounded and excess is surfaced via
	// `dropped`, never dispatched silently (ISI-5265 req 4).
	dispatchCap := maxMentionDispatchPerMessage
	if broadcast {
		dispatchCap = maxBroadcastDispatchPerMessage
	}
	for _, agent := range resolved {
		if len(targets) >= dispatchCap {
			dropped++ // rate cap: count the drop so the caller can log a non-silent truncation
			continue
		}
		targets = append(targets, MentionDispatch{
			AgentName: agent.Name, // canonical roster casing
			HopDepth:  nextHop,
		})
	}
	return targets, dropped, false
}

// partyRouteClass classifies how a committed message's auto-dispatch is routed with respect to the
// ADR-0031 Ruling B facilitator-vs-broadcast decision (ISI-5639 C2). It is derived PURELY (no I/O), like
// resolveMentionTargets, so the handler can decide whether to open/feed a session BEFORE taking the DB
// action and every branch is unit-testable without Postgres.
type partyRouteClass int

const (
	// partyRouteNormal — handle via the ordinary resolveMentionTargets/DispatchMentionsFrom path exactly
	// as before. Covers everything that is NOT a default bare-party debate: a direct:/@-mention post, an
	// agent-authored post, a non-text kind, a room with <2 eligible agents or no dispatchable coordinator,
	// and — crucially — the EXPLICIT demoted-broadcast affordance (§3.2), which keeps its legacy fan-out.
	partyRouteNormal partyRouteClass = iota
	// partyRouteFacilitator — a human bare-party text post (audience 'party', no @-mention, no explicit
	// broadcast flag) in a room with a dispatchable Coordinator AND ≥2 eligible agents. The caller OPENs
	// (or feeds) a facilitator session and lets the coordinator sequence turns (ISI-5638 C1) instead of
	// the legacy simultaneous broadcast — closing the ISI-5587 thundering-herd path at its root (§3.2).
	partyRouteFacilitator
)

// classifyPartyRoute decides whether a just-committed message should route to the sequenced facilitator
// (ADR-0031 Ruling B) rather than the legacy simultaneous bare-party broadcast. It is the C2 counterpart
// to resolveMentionTargets: that function still owns @-mention / direct / explicit-broadcast dispatch;
// this one only re-routes the DEFAULT human bare-party post. Every other shape returns partyRouteNormal,
// so the caller falls through to the unchanged dispatch path and no existing behaviour moves.
func classifyPartyRoute(msg *Message, roster []TeamAgent) partyRouteClass {
	if msg == nil {
		return partyRouteNormal
	}
	// Human-authored only: an agent bare-party post already dispatches nobody (loop-safety), and only a
	// human may open a session (PartyStartAllowed) — the anti-N² guarantee at the entry point (§5.2).
	if msg.AuthorAgentID != nil {
		return partyRouteNormal
	}
	// Bare PARTY post: audience 'party' (not a direct: reply). A direct:<agent> post is a targeted
	// one-to-one reply, never a whole-room debate — unchanged (§3.2).
	if msg.Audience != "party" {
		return partyRouteNormal
	}
	// Only the plain discussion kind opens a debate; a structured/proposal/decision/vote party post keeps
	// its legacy behaviour (ADR-0031 §3.2 targets kind='text'). Empty == the normalizeKind 'text' default.
	if msg.Kind != "" && msg.Kind != "text" {
		return partyRouteNormal
	}
	// A single/explicit @-mention is a targeted reply or a coordinator hand-off (D4), not a bare-party
	// debate — resolveMentionTargets owns it unchanged (§3.2: single direct @-mention → direct dispatch).
	if len(parseMentions(msg.Body)) > 0 {
		return partyRouteNormal
	}
	// Explicit demoted broadcast affordance (§3.2): the caller opted into the one-shot parallel
	// ask-everyone path — honour it (subject to the fan-out cap + ISI-5594 admission), do NOT re-route.
	if partyBroadcastRequested(msg.Payload) {
		return partyRouteNormal
	}
	// The facilitator path needs a dispatchable Coordinator to sequence the round; without one the debate
	// cannot run, so fall back to the legacy broadcast rather than open an inert session (demote, not delete).
	if _, ok := coordinatorOf(roster); !ok {
		return partyRouteNormal
	}
	// Count the eligible (dispatchable) agents the legacy path WOULD have broadcast to. ≥2 ⇒ a real
	// multi-agent room ⇒ sequence it; a 1-eligible-agent room stays a single direct dispatch (§3.2 test).
	eligible := 0
	for _, a := range roster {
		if a.Name != "" && dispatchableStatus(a.Status) {
			eligible++
		}
	}
	if eligible < 2 {
		return partyRouteNormal
	}
	return partyRouteFacilitator
}

// coordinatorOf returns the Team's single Coordinator-role agent (Role.Spec.Coordinator, ISI-4431 —
// the Team-admission webhook enforces ≤1 per Team) if one is on the roster AND dispatchable. ok=false
// when the Team configures no coordinator role, or its coordinator is opted out (paused/blocked) —
// either way the multi-mention caller must fall back to a picker (D3) rather than fan out (ISI-5283).
func coordinatorOf(roster []TeamAgent) (TeamAgent, bool) {
	for _, a := range roster {
		if a.Coordinator && a.Name != "" && dispatchableStatus(a.Status) {
			return a, true
		}
	}
	return TeamAgent{}, false
}

// DispatchMentionsFrom is the shared post-commit trigger BOTH room write edges call (ISI-5125): the
// apiserver REST Handler.postMessage and the cmd/memory discussion_post MCP tool (which runs in a separate
// process and posts via discussion.Store directly). It resolves the auto-dispatch targets for a
// just-committed message against the supplied roster and emits each through the dispatcher seam. Hoisting
// it here — with the seams and the already-resolved roster passed in — is what keeps the two edges from
// drifting: neither re-implements the guardrail application or the MentionDispatch field wiring. It is
// best-effort by construction (the message is already durable, so a dispatch failure never fails the
// write) and no-ops when the dispatcher is nil (an unwired room stays coordination-free).
func DispatchMentionsFrom(ctx context.Context, dispatcher MentionDispatcher, roster []TeamAgent, projectID string, auth AuthorContext, msg *Message) {
	if dispatcher == nil || msg == nil {
		return
	}
	targets, dropped, fallback := resolveMentionTargets(msg, roster)
	if fallback {
		// Multi-mention (2+) post on a Team with no dispatchable Coordinator role (ISI-5283 D3). We
		// deliberately dispatch NOBODY rather than silently fanning out to every mentioned agent; the
		// console submit surface turns this into a lightweight picker/prompt. Surface it here (the sole
		// caller) so the non-dispatch is visible in the logs rather than looking like a dropped post.
		slog.InfoContext(ctx, "discussion: multi-mention with no coordinator — fan-out suppressed, picker expected",
			"projectID", projectID, "messageID", msg.ID)
		return
	}
	if dropped > 0 {
		// Non-silent truncation (the fan-out/broadcast cap guardrail): the message @-mentioned — or, for
		// a human party post, the room held — more dispatchable agents than the cap allows, so the tail
		// was not dispatched. Surface it here (the sole caller) so an over-cap broadcast is visible in the
		// logs rather than silently clipped.
		slog.WarnContext(ctx, "discussion: mention dispatch fan-out capped",
			"projectID", projectID, "messageID", msg.ID, "dispatched", len(targets), "dropped", dropped)
	}
	for _, t := range targets {
		t.ProjectID = projectID
		t.ThreadID = msg.ThreadID
		t.MessageID = msg.ID
		t.TeamID = auth.TeamID
		t.TriggeredByPrincipal = auth.Principal
		t.TriggeredByAgentID = auth.AgentID
		_ = dispatcher.DispatchMention(ctx, t) // best-effort: the row is already committed
	}
}

// StampReplyHopFrom is the shared pre-write reply-hop stamp BOTH room write edges call (ISI-5125). It
// returns the payload a message should be stored with, auto-stamping the loop-guard hop for an
// agent-authored reply posted from within a dispatched thread-run. The hop is derived from the Run's
// server-stamped identity (auth.RunID) via the dispatch ledger — NOT from an agent-supplied number — so a
// misbehaving run cannot reset its own hop to escape the loop guard. A non-agent post, a post outside a
// Run, an unwired resolver, or a Run that is not a thread-run all pass the payload through unchanged (a
// normal post stays hop 0). Best-effort: a resolver error also passes through — the reply is already valid,
// and the other three guardrails still bound the blast radius.
func StampReplyHopFrom(ctx context.Context, resolver ReplyHopResolver, auth AuthorContext, payload *json.RawMessage) *json.RawMessage {
	if auth.AgentID == nil || auth.RunID == nil || resolver == nil {
		return payload
	}
	hop, ok, err := resolver.HopForDispatchedRun(ctx, *auth.RunID)
	if err != nil || !ok {
		return payload
	}
	stamped := StampDispatchHop(payload, hop)
	return &stamped
}

// dispatchMentions is the REST handler's post-commit hook: it resolves the room roster (admin ⇒ the
// project namespace's agents, everyone else ⇒ their own Team — an agent-authored trigger is never admin,
// so it resolves its own squad, the tenancy the room lives in) and delegates to the shared
// DispatchMentionsFrom so the REST and MCP-tool edges cannot drift. No-ops when unwired (nil
// dispatcher/roster source).
func (h *Handler) dispatchMentions(ctx context.Context, projectID string, auth AuthorContext, msg *Message) {
	if h.dispatcher == nil || h.org == nil || msg == nil {
		return
	}
	roster, err := h.projectRoster(ctx, projectID, auth)
	if err != nil {
		return // the roster is a projection, not a fence — a read failure degrades to no dispatch
	}
	// ISI-5615 (P2 mint-gate): try the party-facilitator path first. If this message is a
	// coordinator's @-mention dispatch in an active party session, it handles voice dispatch
	// (bypassing D4, applying VoicesAllowedThisRound, assembling PartyContext) and returns true.
	// On false the message is not a party facilitator post — fall through to the normal path.
	if h.dispatchPartyMentions(ctx, projectID, auth, msg, roster) {
		return
	}
	// ISI-5639 (ADR-0031 Ruling B): a human bare-party text post in a ≥2-eligible room with a coordinator
	// routes to the sequenced facilitator (open/feed a session) instead of the legacy simultaneous
	// broadcast — closing the ISI-5587 thundering-herd path at its root. On false (not a facilitator-route
	// post, or the open failed) fall through to the unchanged broadcast / @-mention / direct dispatch.
	if h.routeBarePartyToFacilitator(ctx, projectID, auth, msg, roster) {
		return
	}
	DispatchMentionsFrom(ctx, h.dispatcher, roster, projectID, auth, msg)
}

// stampReplyHop (ISI-5116) is the REST handler's pre-write hook; it delegates to the shared
// StampReplyHopFrom (ISI-5125) so the REST and MCP-tool edges stamp the loop-guard hop identically.
func (h *Handler) stampReplyHop(ctx context.Context, auth AuthorContext, payload *json.RawMessage) *json.RawMessage {
	return StampReplyHopFrom(ctx, h.hopResolver, auth, payload)
}
