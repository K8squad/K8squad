package discussion

import (
	"context"
	"encoding/json"
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
// Audience semantics (plan): `direct:<agent>` dispatches ONLY that agent; a `party` post dispatches
// each @-mentioned agent; a bare `party` post with no @-mention dispatches nobody (so a plain message
// never wakes the whole squad).

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

const (
	// maxMentionHopDepth bounds agent→agent chaining: a human post is hop 0, so the run it dispatches
	// is hop 1; that run's @-mention dispatches hop 2; a hop-2 run's @-mention would be hop 3 and is
	// refused. This is the plan's "max 2 hops per human turn" — enough for a PM→implementer→reviewer
	// exchange, closed before an unbounded paid loop.
	maxMentionHopDepth = 2

	// maxMentionDispatchPerMessage caps the fan-out of a single post (rate/cost guardrail): a message
	// that @-mentions the whole squad dispatches at most this many agents. Excess mentions are dropped.
	maxMentionDispatchPerMessage = 5

	// dispatchPayloadKey is the message-payload field carrying the in-band loop-guard hop counter
	// (plan: "a per-thread hop counter in payload"). Human posts omit it (hop 0); a dispatched run
	// stamps it on its reply so the NEXT mention parse sees the accumulated depth.
	dispatchPayloadKey = "_dispatch"
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
func resolveMentionTargets(msg *Message, roster []TeamAgent) (targets []MentionDispatch, dropped int) {
	// (1) Loop guard: hop this message sits at (human post ⇒ 0). The run a mention here dispatches
	// runs at hop+1; if that would exceed the cap, an agent-authored message dispatches nobody — the
	// paid-loop backstop. A human post always passes (0+1 ≤ cap).
	msgHop := 0
	if msg.AuthorAgentID != nil {
		msgHop = DispatchHopOf(msg.Payload)
	}
	nextHop := msgHop + 1
	if nextHop > maxMentionHopDepth {
		return nil, 0
	}

	// (2) Audience routing: direct:<agent> targets exactly that agent (ignoring body @-tokens); a
	// party post targets its @-mentions; a bare party post targets nobody.
	var candidates []string
	if target, ok := strings.CutPrefix(msg.Audience, "direct:"); ok && target != "" {
		candidates = []string{target}
	} else {
		candidates = parseMentions(msg.Body)
	}
	if len(candidates) == 0 {
		return nil, 0
	}

	// (3) Resolve each candidate against the roster (case-insensitive), applying the opt-out and
	// self-mention guardrails, de-duplicating, and capping the fan-out.
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
		if len(targets) >= maxMentionDispatchPerMessage {
			dropped++ // rate cap: count the drop so the caller can log a non-silent truncation
			continue
		}
		targets = append(targets, MentionDispatch{
			AgentName: agent.Name, // canonical roster casing
			HopDepth:  nextHop,
		})
	}
	return targets, dropped
}

// dispatchMentions is the post-commit hook postMessage calls: it resolves the auto-dispatch targets
// for a just-committed message and emits each through the dispatcher seam. It is best-effort by
// construction — the message is already durable, so a dispatch failure never fails the HTTP write (the
// human's post must not be lost because a paid run could not be minted). No-ops when the feature is
// unwired (nil dispatcher/roster), keeping DB-less runs coordination-free exactly as before.
func (h *Handler) dispatchMentions(ctx context.Context, projectID string, auth AuthorContext, msg *Message) {
	if h.dispatcher == nil || h.org == nil || msg == nil {
		return
	}
	// The dispatch scope mirrors the roster right-rail (ISI-5107): admin ⇒ the project namespace's
	// agents, everyone else ⇒ their own Team. An agent-authored trigger is never admin, so it resolves
	// its own squad — the same tenancy the room lives in.
	roster, err := h.projectRoster(ctx, projectID, auth)
	if err != nil {
		return // the roster is a projection, not a fence — a read failure degrades to no dispatch
	}
	targets, _ := resolveMentionTargets(msg, roster)
	for _, t := range targets {
		t.ProjectID = projectID
		t.ThreadID = msg.ThreadID
		t.MessageID = msg.ID
		t.TeamID = auth.TeamID
		t.TriggeredByPrincipal = auth.Principal
		t.TriggeredByAgentID = auth.AgentID
		_ = h.dispatcher.DispatchMention(ctx, t) // best-effort: the row is already committed
	}
}
