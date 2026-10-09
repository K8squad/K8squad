// ISI-5586 (ISI-5569 WS-C, ruling ADR-0027 §3.2/§3.3) — cross-talk context assembly + persona prompt.
//
// WS-B (party.go) gave party mode a durable, budgeted session. But a dispatched voice run still only
// received a bare "read the thread, reply in the room" instruction string (mentiondispatch.mentionRunBody):
// it had to PULL the other voices' turns itself (discussion_search / GET thread), and nothing framed it as
// a named persona with a mandate to actually disagree. The result read as N independent monologues, not a
// debate.
//
// This file is the engine-side ASSEMBLY + RENDER unit the facilitator (WS-D) feeds each voice:
//
//   - PersonaBlurb: the dispatched agent's identity (name / role / icon / one-line identity /
//     communication style), sourced from roster metadata by WS-D.
//   - PartyPeerTurn + AssemblePeerTurns: the round's OTHER voices, turned into icon-prefixed turns for a
//     "What Others Said This Round" block — pushing the right context instead of relying on pull.
//   - RollSummary + ShouldRefreshSummary: a rolling <400-word digest of the debate so far, refreshed every
//     2–3 rounds / on topic shift (not the full transcript — the full history is one GET away; this is the
//     cheap always-present frame).
//   - PartyContext.RenderVoiceContext: the persona prompt proper — named/icon-prefixed framing, the MANDATE
//     to disagree ("don't hedge"), the PERMISSION to pass ("one sentence, don't manufacture an opinion").
//
// Everything here is PURE (no I/O): WS-D resolves the roster + reads the round's messages from the store,
// then hands this unit already-materialised inputs, so the assembly and the summary-rolling are
// exhaustively unit-testable without Postgres. The rendered block rides into the dispatched voice run via
// MentionDispatch.Party (consumed by internal/mentiondispatch.mentionRunBody).
package discussion

import (
	"fmt"
	"strings"
)

const (
	// MaxSummaryWords is the hard word cap on the rolling discussion summary (ADR-0027 §3.2 / WS-C:
	// "rolling <400-word summary"). It is a frame, not the transcript — the full history stays one GET
	// away; this keeps the always-injected context cheap.
	MaxSummaryWords = 400

	// SummaryRefreshInterval folds a new round digest into the rolling summary every N rounds (WS-C:
	// "updated every 2–3 rounds"). With N=2 the summary refreshes on rounds 1, 2, 4, 6… and coasts on 3,
	// 5… — i.e. at least every 2–3 rounds — plus an unconditional refresh on a topic shift. Keeping the
	// summary slightly stale between refreshes is intentional: the latest round is always rendered
	// verbatim in "What Others Said This Round", so the summary only has to carry the compressed older
	// history.
	SummaryRefreshInterval = 2
)

// Persona prompt guidelines (ADR-0027 §3.2 WS-C). These are the canonical texts so the voice body, the
// facilitator body (WS-D), and the tests all share one source of truth rather than drifting copies.
const (
	// DisagreeMandate is the anti-consensus instruction: a party debate that converges to polite
	// agreement is worthless, so a voice is REQUIRED to name disagreement plainly.
	DisagreeMandate = "If you disagree with another voice, say so plainly and say why — do NOT hedge, " +
		"soften, or paper over the disagreement to keep the peace. A debate where everyone agrees was not worth paying for."

	// PassPermission is the escape hatch that keeps the mandate honest: a voice with nothing to add must
	// be free to pass in one sentence rather than manufacture a contrarian take just to fill its turn.
	PassPermission = "If you genuinely have nothing to add this round, say so in ONE sentence (e.g. " +
		"\"No objection — I'd defer to the others here.\") rather than manufacturing an opinion to fill the turn."

	// FacilitatorNoParaphrase is the facilitator-side rule (surfaced here as the shared source of truth
	// for WS-D's facilitator body): the facilitator frames and routes the debate but NEVER restates a
	// voice's point in its own words — paraphrase launders the distinct voices back into one, the exact
	// Option-C failure mode ADR-0027 §2 rejected.
	FacilitatorNoParaphrase = "You are the facilitator: frame the question and select who speaks, but NEVER " +
		"paraphrase or summarise a voice's contribution in your own words — let each voice speak for itself verbatim."
)

// PersonaBlurb is a party voice's identity, sourced from roster metadata by WS-D (the dispatched agent's
// assignable name, the role it fills, and — best-effort — an icon glyph, a one-line identity/charter, and
// a communication-style note). Only Name is required; the render degrades gracefully as fields are empty.
type PersonaBlurb struct {
	Name               string `json:"name"`                         // the agent's assignable name (the @-token)
	Role               string `json:"role,omitempty"`               // Agent.Spec.RoleRef.Name, e.g. "Architect"
	Icon               string `json:"icon,omitempty"`               // optional glyph prefixing this voice's turns
	Identity           string `json:"identity,omitempty"`           // one-line identity/charter from roster metadata
	CommunicationStyle string `json:"communicationStyle,omitempty"` // how this persona speaks (terse, Socratic, …)
}

// label renders the persona as an icon-prefixed, role-qualified name — "🏛 Winston (Architect)" — the
// same shape used for both the dispatched voice's self-framing and each peer turn's attribution.
func (p PersonaBlurb) label() string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "(unnamed)"
	}
	var b strings.Builder
	if icon := strings.TrimSpace(p.Icon); icon != "" {
		b.WriteString(icon)
		b.WriteString(" ")
	}
	b.WriteString(name)
	if role := strings.TrimSpace(p.Role); role != "" {
		fmt.Fprintf(&b, " (%s)", role)
	}
	return b.String()
}

// PartyPeerTurn is one OTHER voice's contribution in the current round, attributed by persona — the unit
// of the "What Others Said This Round" block.
type PartyPeerTurn struct {
	Persona PersonaBlurb `json:"persona"`
	Body    string       `json:"body"`
}

// PartyContext is the cross-talk context pushed into a party-dispatched voice's work-item body (ADR-0027
// §3.2/§3.3). WS-D assembles it per voice per round and attaches it to MentionDispatch.Party; it is nil
// for every non-party dispatch, so the ordinary single-agent / orchestration bodies are untouched.
type PartyContext struct {
	Round          int             `json:"round"`                    // 1-based facilitator round this voice runs in
	Topic          string          `json:"topic,omitempty"`          // the debate's topic (the party_start framing)
	Persona        PersonaBlurb    `json:"persona"`                  // the DISPATCHED voice's own identity
	PeersThisRound []PartyPeerTurn `json:"peersThisRound"`           // what the other voices said this round
	RollingSummary string          `json:"rollingSummary,omitempty"` // <400-word digest of the debate so far
}

// AssemblePeerTurns turns a round's messages into the "What Others Said This Round" turns for the voice
// being dispatched (ADR-0027 §3.3 — the facilitator PUSHES the round's cross-talk instead of each voice
// pulling it). It keeps only agent-authored, non-empty turns, EXCLUDES the dispatched voice's own posts
// (a voice does not need to be told what it itself just said), preserves first-seen order, and resolves
// each author's persona via the supplied roster lookup (WS-D's roster resolver). A nil lookup, or a
// lookup that returns a blank name, falls back to the raw author agent id so a turn is never dropped for
// want of metadata.
func AssemblePeerTurns(roundMsgs []Message, dispatchedAgent string, persona func(agentID string) PersonaBlurb) []PartyPeerTurn {
	turns := make([]PartyPeerTurn, 0, len(roundMsgs))
	for _, m := range roundMsgs {
		if m.AuthorAgentID == nil { // a human framing turn is not a peer voice
			continue
		}
		author := *m.AuthorAgentID
		if author == "" || author == dispatchedAgent {
			continue
		}
		body := strings.TrimSpace(m.Body)
		if body == "" {
			continue
		}
		var p PersonaBlurb
		if persona != nil {
			p = persona(author)
		}
		if strings.TrimSpace(p.Name) == "" {
			p.Name = author
		}
		turns = append(turns, PartyPeerTurn{Persona: p, Body: body})
	}
	return turns
}

// ShouldRefreshSummary reports whether the rolling summary should fold in this round's digest (WS-C:
// "updated every 2–3 rounds / on topic shift"). It refreshes on the first round (to seed the summary),
// unconditionally on a topic shift, and otherwise every SummaryRefreshInterval rounds — so between
// refreshes the summary coasts while the verbatim "What Others Said This Round" block carries the latest
// turns.
func ShouldRefreshSummary(round int, topicShift bool) bool {
	if topicShift || round <= 1 {
		return true
	}
	return round%SummaryRefreshInterval == 0
}

// RollSummary produces the next rolling summary. When refresh is false the prior summary is carried
// forward unchanged (word-capped defensively); when true, this round's digest is appended and the result
// is trimmed to the most recent MaxSummaryWords words — recent-weighted, so a long debate's summary stays
// bounded and favours where the discussion currently is over where it started. Blank digests never grow
// the summary.
func RollSummary(previous, roundDigest string, refresh bool) string {
	if !refresh {
		return keepLastWords(strings.TrimSpace(previous), MaxSummaryWords)
	}
	prev := strings.TrimSpace(previous)
	digest := strings.TrimSpace(roundDigest)
	switch {
	case prev == "" && digest == "":
		return ""
	case prev == "":
		return keepLastWords(digest, MaxSummaryWords)
	case digest == "":
		return keepLastWords(prev, MaxSummaryWords)
	default:
		return keepLastWords(prev+" "+digest, MaxSummaryWords)
	}
}

// keepLastWords returns the trailing max whitespace-separated words of s (the whole string when it has
// fewer), re-joined with single spaces. Dropping from the FRONT keeps the summary recent-weighted.
func keepLastWords(s string, max int) string {
	if max <= 0 {
		return ""
	}
	words := strings.Fields(s)
	if len(words) <= max {
		return strings.Join(words, " ")
	}
	return strings.Join(words[len(words)-max:], " ")
}

// RenderVoiceContext renders the persona prompt + cross-talk block injected into a dispatched voice's
// work-item body (ADR-0027 §3.2 WS-C). It is the context ABOVE the dispatch footer (read-thread / reply /
// hop), which internal/mentiondispatch owns. Sections degrade gracefully: an empty peer set or summary
// prints an explicit "nothing yet" line rather than a dangling header, so round 1's first voice still
// gets a coherent frame.
func (c *PartyContext) RenderVoiceContext() string {
	var b strings.Builder

	// Persona self-framing — named/icon-prefixed, role-qualified.
	fmt.Fprintf(&b, "You are %s, one voice in a facilitated team discussion", c.Persona.label())
	if topic := strings.TrimSpace(c.Topic); topic != "" {
		fmt.Fprintf(&b, " on: «%s»", topic)
	}
	b.WriteString(".\n")
	if id := strings.TrimSpace(c.Persona.Identity); id != "" {
		fmt.Fprintf(&b, "Your charter: %s\n", id)
	}
	if style := strings.TrimSpace(c.Persona.CommunicationStyle); style != "" {
		fmt.Fprintf(&b, "Your voice: %s\n", style)
	}
	if c.Round > 0 {
		fmt.Fprintf(&b, "This is round %d of the discussion.\n", c.Round)
	}

	// Rolling summary of the debate so far (compressed older history).
	b.WriteString("\n## Discussion so far (rolling summary)\n")
	if s := strings.TrimSpace(c.RollingSummary); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	} else {
		b.WriteString("(nothing yet — this is the opening of the discussion)\n")
	}

	// What the other voices said THIS round (verbatim, attributed by persona).
	b.WriteString("\n## What Others Said This Round\n")
	if len(c.PeersThisRound) == 0 {
		b.WriteString("(you are the first to speak this round — react to the summary and topic above)\n")
	} else {
		for _, t := range c.PeersThisRound {
			fmt.Fprintf(&b, "- %s: %s\n", t.Persona.label(), strings.TrimSpace(t.Body))
		}
	}

	// Persona guidelines — mandate to disagree + permission to pass.
	b.WriteString("\n## How to take your turn\n")
	fmt.Fprintf(&b, "- %s\n", DisagreeMandate)
	fmt.Fprintf(&b, "- %s\n", PassPermission)
	b.WriteString("- Speak in your own voice and from your own role — do not restate or endorse another voice just to agree.\n")

	return b.String()
}
