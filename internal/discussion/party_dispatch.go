// ISI-5615 (ISI-5569 WS-D.1, ADR-0027 §3.3) — party-mode facilitator dispatch (P2 mint-gate).
// ISI-5638 (ISI-5624 C1, ADR-0031 Ruling A §3.1) — narrows the per-round fan-out from N→1 (sequenced).
//
// When the Coordinator (facilitator) posts a party @-mention message the normal mention dispatch
// fires BUT hits D4 (multi-mention → coordinator redirect), which short-circuits because the
// coordinator IS the author — treating itself as unavailable and returning the D3 picker fallback.
// That would leave voices un-dispatched.
//
// This file is the party-aware path the REST handler calls FIRST: it detects the coordinator-facilitator
// case, bypasses D4, resolves the @-mentioned voices against the roster (opt-out / self-exclusion /
// de-dupe), caps them via VoicesAllowedThisRound (the server budget gate, not agent-trusted), and RECORDS
// the round — the facilitator message id AND the ordered, capped voice roster — via SetRoundFacilitatorMessage.
//
// It no longer dispatches the voices itself. Under ADR-0031 Ruling A the voices take turns STRICTLY
// sequentially (one run in flight at a time, each reacting to the prior's freshly-landed reply), which can
// only be driven off the ADR-0020 settle markers — i.e. by the operator advancer, not synchronously in this
// one HTTP handler. So this hook resolves + persists the roster; the advancer walks RoundVoices one turn at
// a time (advancer_runner.go dispatchVoice). Only after this returns false (not a party facilitator post)
// does the handler fall through to the normal DispatchMentionsFrom path.
package discussion

import (
	"context"
	"log/slog"
	"strings"
)

// dispatchPartyMentions handles the party-facilitator path. It returns true when it handled the post
// (whether or not any voices were recorded — best-effort throughout, the message is already durable), and
// false when this is not a party facilitator post, in which case the caller should fall through to the
// normal DispatchMentionsFrom path.
//
// The detection criteria (all must hold):
//  1. The message was posted by an agent (author is coordinator, never a human).
//  2. There is an active party session for the thread.
//  3. The posting agent is the Team's Coordinator role.
//  4. The message has at least one resolvable @-mention (otherwise there is nothing to dispatch).
func (h *Handler) dispatchPartyMentions(ctx context.Context, projectID string, auth AuthorContext, msg *Message, roster []TeamAgent) bool {
	if h.dispatcher == nil || h.store == nil || msg == nil {
		return false
	}
	// (1) Only agent authors can be coordinators.
	if auth.AgentID == nil {
		return false
	}
	// (2) Active party session for this thread?
	sess, err := h.store.ActivePartySession(ctx, msg.ThreadID)
	if err != nil {
		return false // ErrNoActivePartySession or real error — not party mode, fall through
	}

	// (3) Is the posting agent the Team's Coordinator role?
	coord, ok := coordinatorOf(roster)
	if !ok {
		return false
	}
	if !strings.EqualFold(coord.Name, *auth.AgentID) {
		return false // author is not the coordinator — ordinary voice post, fall through
	}

	// (4) Resolve @-mentions (no D4, no hop cap — the facilitator dispatches voices directly).
	mentions := parseMentions(msg.Body)
	if len(mentions) == 0 {
		return false // no mentions in body — not a dispatch post
	}
	byName := make(map[string]TeamAgent, len(roster))
	for _, a := range roster {
		if a.Name != "" {
			byName[strings.ToLower(a.Name)] = a
		}
	}
	selfKey := strings.ToLower(*auth.AgentID)
	var resolved []TeamAgent
	seen := make(map[string]bool)
	for _, name := range mentions {
		key := strings.ToLower(name)
		if seen[key] || key == selfKey {
			continue // dedup + self-exclusion
		}
		a, found := byName[key]
		if !found || !dispatchableStatus(a.Status) {
			continue
		}
		seen[key] = true
		resolved = append(resolved, a)
	}
	if len(resolved) == 0 {
		return true // coordinator posted but mentioned nobody dispatchable — handled (no dispatch)
	}

	// Apply VoicesAllowedThisRound budget cap (server-enforced, ADR-0027 §3.3).
	allowed, capped := sess.VoicesAllowedThisRound(len(resolved))
	if capped > 0 {
		slog.WarnContext(ctx, "discussion: party voice dispatch capped by session budget",
			"sessionID", sess.ID, "round", sess.Round,
			"requested", len(resolved), "allowed", allowed, "capped", capped)
		resolved = resolved[:allowed]
	}
	if allowed == 0 {
		return true // budget exhausted — handled (no dispatch)
	}

	// Record this message as the round's facilitator dispatch message AND stamp the ordered, capped voice
	// roster the advancer will walk (first-writer-wins, CAS; ADR-0031 Ruling A §3.1). Best-effort on error:
	// the message is already durable, but without the stamp RoundVoiceSettlement can't key completeness and
	// the advancer has no roster to sequence — so the round would stall until the facilitator-post timeout.
	voices := make([]string, 0, len(resolved))
	for _, a := range resolved {
		voices = append(voices, a.Name) // canonical roster casing, facilitator's @-mention order
	}
	_, won, serr := h.store.SetRoundFacilitatorMessage(ctx, sess.ID, sess.Round, msg.ID, voices)
	if serr != nil {
		slog.WarnContext(ctx, "discussion: party SetRoundFacilitatorMessage failed (best-effort)",
			"sessionID", sess.ID, "round", sess.Round, "messageID", msg.ID, "err", serr)
		return true
	}
	if !won {
		// Another writer (e.g. a concurrent request) won the CAS — do not double-record.
		slog.InfoContext(ctx, "discussion: party facilitator message already set for this round — skip",
			"sessionID", sess.ID, "round", sess.Round)
		return true
	}

	// The voices are NOT dispatched here. ADR-0031 Ruling A: they take turns strictly sequentially (one run
	// in flight, each reacting to the prior's reply), which is driven off settle markers by the operator
	// advancer — it walks RoundVoices one turn at a time, pushing each voice the cross-talk context of every
	// turn taken so far (advancer_runner.go dispatchVoice). This hook's job ends at recording the roster.
	slog.InfoContext(ctx, "discussion: party round roster recorded — advancer will sequence voices",
		"sessionID", sess.ID, "round", sess.Round, "voices", len(voices))
	return true
}

// routeBarePartyToFacilitator implements ADR-0031 Ruling B (ISI-5639 C2): a human bare-party text post in
// a room with a dispatchable Coordinator and ≥2 eligible agents OPENS (or feeds) a facilitator session and
// lets the coordinator sequence turns (ISI-5638 C1), instead of the legacy resolveMentionTargets broadcast
// that fanned the whole roster onto the one endpoint slot at once (the ISI-5587 thundering herd). It returns
// true when it handled the post (a session was opened or an active one was fed), and false when this is not
// a facilitator-route post OR the open failed — in which case the caller falls through to the normal
// DispatchMentionsFrom path so the room is never left silent.
//
// It lives on the REST handler edge only: the MCP discussion_post edge is agent-authored, and an agent
// bare-party post never classifies as facilitator-route (human-only, PartyStartAllowed) — so there is no
// shared decision to hoist, unlike resolveMentionTargets. The decision (classifyPartyRoute) is pure +
// unit-tested; only the open/feed is I/O.
func (h *Handler) routeBarePartyToFacilitator(ctx context.Context, projectID string, auth AuthorContext, msg *Message, roster []TeamAgent) bool {
	if h.store == nil || msg == nil {
		return false
	}
	if classifyPartyRoute(msg, roster) != partyRouteFacilitator {
		return false
	}
	// Open the session on the ALREADY-committed triggering post (its own message IS the topic — no second
	// party_start artifact). Idempotent: an active debate on the thread is FED (opened=false), never
	// re-opened — the new post is already in the thread for the next round to read.
	sess, opened, err := h.store.OpenPartySessionForMessage(ctx, projectID, msg.ThreadID, msg.ID, auth, nil)
	if err != nil {
		// Best-effort: the message is durable. Fall back to the normal dispatch path rather than swallow
		// the human's post — a transient open failure must not leave the room unanswered.
		slog.WarnContext(ctx, "discussion: bare-party facilitator route failed — falling back to normal dispatch",
			"projectID", projectID, "threadID", msg.ThreadID, "messageID", msg.ID, "err", err)
		return false
	}
	if opened {
		slog.InfoContext(ctx, "discussion: bare-party post opened a facilitator session — advancer will sequence voices (not broadcast)",
			"projectID", projectID, "threadID", msg.ThreadID, "sessionID", sess.ID)
	} else {
		slog.InfoContext(ctx, "discussion: bare-party post fed an active facilitator session (no second debate)",
			"projectID", projectID, "threadID", msg.ThreadID, "sessionID", sess.ID)
	}
	return true
}

// flattenMessages returns all messages in the thread tree in breadth-first order, unwrapping the
// nested Replies structure GetThread builds. Used to collect the round's voice posts for the
// "What Others Said This Round" cross-talk block.
func flattenMessages(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	queue := append([]Message(nil), msgs...)
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		out = append(out, m)
		queue = append(queue, m.Replies...)
	}
	return out
}
