// ISI-5615 (ISI-5569 WS-D.1, ADR-0027 §3.3) — party-mode facilitator dispatch (P2 mint-gate).
//
// When the Coordinator (facilitator) posts a party @-mention message the normal mention dispatch
// fires BUT hits D4 (multi-mention → coordinator redirect), which short-circuits because the
// coordinator IS the author — treating itself as unavailable and returning the D3 picker fallback.
// That would leave voices un-dispatched.
//
// This file is the party-aware dispatch path the REST handler calls FIRST: it detects the
// coordinator-facilitator case, bypasses D4, caps the fan-out via VoicesAllowedThisRound (the server
// budget gate, not agent-trusted), assembles the WS-C PartyContext per voice, records the round's
// facilitator message via SetRoundFacilitatorMessage, and dispatches each voice. Only after this
// returns false (not a party facilitator post) does the handler fall through to the normal
// DispatchMentionsFrom path.
package discussion

import (
	"context"
	"log/slog"
	"strings"
)

// dispatchPartyMentions handles the party-facilitator dispatch path. It returns true when it handled
// dispatch (whether or not any voices were actually minted — best-effort throughout, message already
// durable), and false when this is not a party facilitator post, in which case the caller should
// fall through to the normal DispatchMentionsFrom path.
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

	// Record this message as the round's facilitator dispatch message (first-writer-wins, CAS).
	// Best-effort: a failure here means RoundVoiceSettlement can't read completeness, but the
	// message is already durable and the voices will still run.
	_, won, serr := h.store.SetRoundFacilitatorMessage(ctx, sess.ID, sess.Round, msg.ID)
	if serr != nil {
		slog.WarnContext(ctx, "discussion: party SetRoundFacilitatorMessage failed (best-effort)",
			"sessionID", sess.ID, "round", sess.Round, "messageID", msg.ID, "err", serr)
	}
	if !won {
		// Another writer (e.g. a concurrent request) won the CAS — do not double-dispatch.
		slog.InfoContext(ctx, "discussion: party facilitator message already set for this round — skip re-dispatch",
			"sessionID", sess.ID, "round", sess.Round)
		return true
	}

	// Build per-voice PartyContext. PeersThisRound is empty on the first dispatch in a round
	// (no voices have run yet); the rolling summary is currently empty (a later enhancement
	// could seed it from prior rounds). PersonaBlurb uses only Name (roster has no icon/identity
	// fields yet — degrades gracefully per WS-C spec).
	personaOf := func(agentID string) PersonaBlurb {
		a, ok := byName[strings.ToLower(agentID)]
		if !ok {
			return PersonaBlurb{Name: agentID}
		}
		return PersonaBlurb{Name: a.Name}
	}

	// Read the thread's messages to supply the "What Others Said This Round" block for later
	// rounds (round 1 will have empty peers — correct).
	thread, terr := h.store.GetThread(ctx, projectID, auth.TeamID, msg.ThreadID)
	var roundMsgs []Message
	if terr == nil && thread != nil {
		// Collect messages since the previous facilitator post (best approximation of "this round's
		// voices"). Use all thread messages; AssemblePeerTurns filters to agent-authored non-empty.
		roundMsgs = flattenMessages(thread.Messages)
	}

	// Dispatch each voice.
	nextHop := 1 // facilitator is minted at hop 0 (by the advancer, not via mention dispatch)
	for _, voice := range resolved {
		peers := AssemblePeerTurns(roundMsgs, voice.Name, personaOf)
		partyCtx := &PartyContext{
			Round:          sess.Round,
			Persona:        PersonaBlurb{Name: voice.Name},
			PeersThisRound: peers,
		}
		d := MentionDispatch{
			ProjectID:            projectID,
			ThreadID:             msg.ThreadID,
			MessageID:            msg.ID,
			TeamID:               auth.TeamID,
			AgentName:            voice.Name,
			HopDepth:             nextHop,
			TriggeredByPrincipal: auth.Principal,
			TriggeredByAgentID:   auth.AgentID,
			Party:                partyCtx,
		}
		if err := h.dispatcher.DispatchMention(ctx, d); err != nil {
			slog.WarnContext(ctx, "discussion: party voice dispatch failed (best-effort)",
				"sessionID", sess.ID, "round", sess.Round, "agent", voice.Name, "err", err)
		}
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
