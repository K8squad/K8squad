package discussion

// ISI-5586 (ISI-5569 WS-C) — cross-talk context assembly + persona-prompt coverage. Pure unit lane: the
// assembly and the summary-rolling take materialised inputs, so no Postgres.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func partyAgentMsg(agent, body string) Message {
	a := agent
	return Message{ID: uuid.New(), AuthorAgentID: &a, Body: body}
}

func partyHumanMsg(body string) Message {
	return Message{ID: uuid.New(), AuthorPrincipal: "user:henrik", Body: body}
}

// roster fixture — a persona lookup WS-D would supply from CR metadata.
func partyRoster(m map[string]PersonaBlurb) func(string) PersonaBlurb {
	return func(a string) PersonaBlurb { return m[a] }
}

// --- AssemblePeerTurns -------------------------------------------------------

func TestAssemblePeerTurnsExcludesSelfAndHumans(t *testing.T) {
	roster := partyRoster(map[string]PersonaBlurb{
		"winston": {Name: "Winston", Role: "Architect", Icon: "🏛"},
		"ada":     {Name: "Ada", Role: "Engineer"},
	})
	msgs := []Message{
		partyHumanMsg("let's debate the storage engine"), // human framing — not a peer voice
		partyAgentMsg("winston", "I'd use Postgres."),
		partyAgentMsg("ada", "   "),                  // blank — dropped
		partyAgentMsg("robo", "I think about it..."), // the dispatched voice's OWN turn — excluded
		partyAgentMsg("ada", "Disagree — SQLite is enough."),
	}

	turns := AssemblePeerTurns(msgs, "robo", roster)
	if len(turns) != 2 {
		t.Fatalf("got %d peer turns, want 2 (self + human + blank excluded): %+v", len(turns), turns)
	}
	if turns[0].Persona.Name != "Winston" || turns[0].Body != "I'd use Postgres." {
		t.Fatalf("first turn = %+v, want Winston verbatim", turns[0])
	}
	if turns[1].Persona.Name != "Ada" || turns[1].Body != "Disagree — SQLite is enough." {
		t.Fatalf("second turn = %+v, want Ada verbatim", turns[1])
	}
}

func TestAssemblePeerTurnsFallsBackToAgentIDWithoutMetadata(t *testing.T) {
	// A nil lookup, or one that returns a blank name, must never drop the turn — it falls back to the
	// raw author id so cross-talk is never lost for want of roster metadata.
	msgs := []Message{partyAgentMsg("ghost", "I exist.")}
	for _, lookup := range []func(string) PersonaBlurb{nil, partyRoster(nil)} {
		turns := AssemblePeerTurns(msgs, "self", lookup)
		if len(turns) != 1 || turns[0].Persona.Name != "ghost" {
			t.Fatalf("fallback failed: %+v", turns)
		}
	}
}

// --- ShouldRefreshSummary ----------------------------------------------------

func TestShouldRefreshSummaryEvery2to3RoundsOrTopicShift(t *testing.T) {
	cases := []struct {
		round      int
		topicShift bool
		want       bool
		why        string
	}{
		{1, false, true, "round 1 seeds the summary"},
		{2, false, true, "every SummaryRefreshInterval (2)"},
		{3, false, false, "coasts between refreshes"},
		{4, false, true, "refresh again at round 4"},
		{3, true, true, "topic shift forces a refresh even on a coast round"},
	}
	for _, c := range cases {
		if got := ShouldRefreshSummary(c.round, c.topicShift); got != c.want {
			t.Errorf("ShouldRefreshSummary(%d,%v)=%v, want %v (%s)", c.round, c.topicShift, got, c.want, c.why)
		}
	}
}

// --- RollSummary -------------------------------------------------------------

func TestRollSummaryFoldsOnRefreshAndCoastsOtherwise(t *testing.T) {
	// refresh=false → prior summary carried forward unchanged.
	if got := RollSummary("round one recap", "NEW round two digest", false); got != "round one recap" {
		t.Fatalf("coast round should not fold the digest, got %q", got)
	}
	// refresh=true → digest appended after the prior summary.
	got := RollSummary("round one recap", "round two digest", true)
	if !strings.Contains(got, "round one recap") || !strings.Contains(got, "round two digest") {
		t.Fatalf("refresh should fold both, got %q", got)
	}
	if strings.Index(got, "round one") > strings.Index(got, "round two") {
		t.Fatalf("digest must append AFTER the prior summary, got %q", got)
	}
}

func TestRollSummaryCapsToMaxWordsRecentWeighted(t *testing.T) {
	prev := strings.TrimSpace(strings.Repeat("old ", MaxSummaryWords)) // exactly MaxSummaryWords "old"
	got := RollSummary(prev, "fresh tail words here", true)
	words := strings.Fields(got)
	if len(words) > MaxSummaryWords {
		t.Fatalf("summary not capped: %d words > %d", len(words), MaxSummaryWords)
	}
	// Recent-weighted: dropping from the front keeps the freshest words, so the newly folded tail must
	// survive while the oldest "old" tokens are evicted.
	if !strings.HasSuffix(got, "fresh tail words here") {
		t.Fatalf("recent words evicted — cap should drop from the front, got tail %q", got[max(0, len(got)-40):])
	}
}

func TestRollSummaryBlankInputs(t *testing.T) {
	if got := RollSummary("", "", true); got != "" {
		t.Fatalf("empty+empty should stay empty, got %q", got)
	}
	if got := RollSummary("seed", "", true); got != "seed" {
		t.Fatalf("blank digest should not alter the summary, got %q", got)
	}
	if got := RollSummary("", "seed", true); got != "seed" {
		t.Fatalf("seeding an empty summary should take the digest, got %q", got)
	}
}

// --- RenderVoiceContext ------------------------------------------------------

func TestRenderVoiceContextFullFrame(t *testing.T) {
	c := &PartyContext{
		Round: 2,
		Topic: "which storage engine",
		Persona: PersonaBlurb{
			Name: "Ada", Role: "Engineer", Icon: "⚙",
			Identity: "guards runtime simplicity", CommunicationStyle: "terse, concrete",
		},
		RollingSummary: "Round 1: Winston argued for Postgres; Ada pushed SQLite.",
		PeersThisRound: []PartyPeerTurn{
			{Persona: PersonaBlurb{Name: "Winston", Role: "Architect", Icon: "🏛"}, Body: "Postgres scales; commit to it."},
		},
	}
	body := c.RenderVoiceContext()

	for _, want := range []string{
		"⚙ Ada (Engineer)",          // persona self-framing, icon + role
		"which storage engine",      // topic
		"guards runtime simplicity", // identity
		"terse, concrete",           // communication style
		"round 2",                   // round
		"## Discussion so far (rolling summary)",
		"Winston argued for Postgres", // summary content
		"## What Others Said This Round",
		"🏛 Winston (Architect): Postgres scales; commit to it.", // peer turn verbatim, attributed
		"## How to take your turn",
		DisagreeMandate,
		PassPermission,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered voice context missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderVoiceContextEmptyRoundDegradesGracefully(t *testing.T) {
	// Round 1, first speaker, no summary yet — headers must still be coherent, not dangling.
	c := &PartyContext{Round: 1, Persona: PersonaBlurb{Name: "solo"}}
	body := c.RenderVoiceContext()
	if !strings.Contains(body, "nothing yet") {
		t.Fatalf("empty summary should print an explicit placeholder, got:\n%s", body)
	}
	if !strings.Contains(body, "first to speak this round") {
		t.Fatalf("empty peer set should print an explicit placeholder, got:\n%s", body)
	}
	// the mandate/permission guidelines are always present, even for the opening turn.
	if !strings.Contains(body, DisagreeMandate) || !strings.Contains(body, PassPermission) {
		t.Fatal("persona guidelines must always be rendered")
	}
}
