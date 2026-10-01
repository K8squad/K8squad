package discussion

// Dispatch-on-mention coverage (ISI-5108, plan ISI-4919). The decision core (resolveMentionTargets)
// and the post-commit hook (dispatchMentions) are exercised WITHOUT a database — the store is never
// touched, so these ride the default unit lane exactly like the sibling mention-search tests. The
// three plan-mandated integration scenarios are all here: (a) an @-mention dispatches exactly one run
// for the named agent, (b) an agent→agent chain is bounded, (c) direct vs party audience routing. The
// four guardrails (loop / de-dupe / rate-cost / opt-out) each have a dedicated case.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeDispatcher records every MentionDispatch the hook emits and can be forced to error to prove the
// hook is best-effort (a dispatch failure never propagates to the HTTP write).
type fakeDispatcher struct {
	calls []MentionDispatch
	err   error
}

func (f *fakeDispatcher) DispatchMention(_ context.Context, d MentionDispatch) error {
	f.calls = append(f.calls, d)
	return f.err
}

func agentMsg(body, audience, agentID string, payload *json.RawMessage) *Message {
	m := &Message{ID: uuid.New(), ThreadID: uuid.New(), Body: body, Audience: audience, Payload: payload}
	if agentID != "" {
		id := agentID
		m.AuthorAgentID = &id
	}
	return m
}

func names(ts []MentionDispatch) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.AgentName)
	}
	return out
}

// --- parseMentions -----------------------------------------------------------

func TestParseMentions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"plain", "hey @john can you look?", []string{"john"}},
		{"hyphenated agent name", "@Robo-Coder please review", []string{"Robo-Coder"}},
		{"trailing period stripped", "ping @john.", []string{"john"}},
		{"de-dupe case-insensitive", "@John and @john again", []string{"John"}},
		{"multiple distinct, order preserved", "@a then @b then @c", []string{"a", "b", "c"}},
		{"no mentions", "just a plain message", nil},
		{"email is not a mention", "mail me at bob@example.com", nil},
		{"mid-word @ is not a mention", "path/to@thing or a@b", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMentions(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("parseMentions(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseMentions(%q) = %v, want %v", tc.body, got, tc.want)
				}
			}
		})
	}
}

// --- hop counter round-trip --------------------------------------------------

func TestDispatchHopRoundTrip(t *testing.T) {
	if got := DispatchHopOf(nil); got != 0 {
		t.Fatalf("nil payload hop = %d, want 0", got)
	}
	stamped := StampDispatchHop(nil, 2)
	if got := DispatchHopOf(&stamped); got != 2 {
		t.Fatalf("round-trip hop = %d, want 2", got)
	}
	// Stamping preserves an existing payload object rather than clobbering it.
	base := json.RawMessage(`{"foo":"bar"}`)
	merged := StampDispatchHop(&base, 1)
	var got struct {
		Foo      string `json:"foo"`
		Dispatch struct {
			HopDepth int `json:"hopDepth"`
		} `json:"_dispatch"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("merged payload not valid JSON: %v", err)
	}
	if got.Foo != "bar" || got.Dispatch.HopDepth != 1 {
		t.Fatalf("merged = %s, want foo preserved + hop 1", merged)
	}
}

// --- (a) an @-mention dispatches exactly one run for the named agent ---------

func TestResolveDispatchesExactlyOne(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "idle"}}
	msg := agentMsg("hey @john please look", "party", "", nil) // human-authored (no agentID)
	targets, dropped, _ := resolveMentionTargets(msg, roster)
	if dropped != 0 || len(targets) != 1 {
		t.Fatalf("targets = %v (dropped %d), want exactly [john]", names(targets), dropped)
	}
	if targets[0].AgentName != "john" {
		t.Fatalf("dispatched %q, want john", targets[0].AgentName)
	}
	if targets[0].HopDepth != 1 {
		t.Fatalf("human turn dispatches at hop %d, want 1", targets[0].HopDepth)
	}
}

// --- (c) direct vs party audience routing -----------------------------------

func TestResolveAudienceRouting(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "working"}}

	// direct:<agent> dispatches ONLY that agent, ignoring body @-tokens.
	direct := agentMsg("@jane too", "direct:john", "", nil)
	if got := names(mustResolve(t, direct, roster)); len(got) != 1 || got[0] != "john" {
		t.Fatalf("direct routing = %v, want [john] only", got)
	}

	// party + a SINGLE @agent dispatches exactly that agent. (A party post that resolves 2+ mentions
	// now routes to the Team Coordinator instead of fanning out — ISI-5283, covered by the dedicated
	// TestResolveMultiMention* cases below.)
	party := agentMsg("@john please take this", "party", "", nil)
	if got := names(mustResolve(t, party, roster)); len(got) != 1 || got[0] != "john" {
		t.Fatalf("party routing = %v, want [john]", got)
	}

	// A bare HUMAN party post with no @-mention now BROADCASTS to the whole room (ISI-5265) — see
	// TestResolveBroadcastHumanPartyWakesWholeRoom for the dedicated coverage. A bare AGENT party post
	// still dispatches nobody (loop-safety) — see TestResolveBroadcastAgentPartyDispatchesNobody.
}

// --- ISI-5265: human party + no @-mention broadcasts to the whole room -------

func TestResolveBroadcastHumanPartyWakesWholeRoom(t *testing.T) {
	roster := []TeamAgent{
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "idle"},
		{Name: "amy", Status: "offline"},
	}
	// Human-authored (no agentID), party audience, no @-mention → every eligible agent is dispatched.
	msg := agentMsg("standup in 5, whole squad please", "party", "", nil)
	targets, dropped, _ := resolveMentionTargets(msg, roster)
	if dropped != 0 || len(targets) != 3 {
		t.Fatalf("broadcast = %v (dropped %d), want all three [john jane amy]", names(targets), dropped)
	}
	for _, tg := range targets {
		if tg.HopDepth != 1 {
			t.Fatalf("broadcast target %q hop = %d, want 1 (human hop-0 turn)", tg.AgentName, tg.HopDepth)
		}
	}
}

// --- ISI-5265: an AGENT bare party post dispatches nobody (loop-safety) ------

func TestResolveBroadcastAgentPartyDispatchesNobody(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "working"}}
	// Agent-authored (agentID set), party audience, no @-mention → NOBODY. Broadcast is a hop-0 human
	// privilege only; an agent bare party post must never N²-loop the whole squad.
	msg := agentMsg("just narrating my work", "party", "jane", nil)
	if got := mustResolve(t, msg, roster); len(got) != 0 {
		t.Fatalf("agent bare party = %v, want nobody (loop-safety)", names(got))
	}
}

// --- ISI-5265: broadcast still honours opt-out ------------------------------

func TestResolveBroadcastExcludesPausedAgents(t *testing.T) {
	roster := []TeamAgent{
		{Name: "john", Status: "paused"},
		{Name: "jane", Status: "blocked"},
		{Name: "amy", Status: "disabled"},
		{Name: "bob", Status: "working"},
	}
	msg := agentMsg("whole room, heads up", "party", "", nil)
	got := names(mustResolve(t, msg, roster))
	if len(got) != 1 || got[0] != "bob" {
		t.Fatalf("broadcast opt-out = %v, want only the dispatchable agent bob", got)
	}
}

// --- ISI-5265: broadcast fan-out cap enforced with a non-silent drop --------

func TestResolveBroadcastRateCap(t *testing.T) {
	roster := make([]TeamAgent, 0, maxBroadcastDispatchPerMessage+4)
	for i := 0; i < maxBroadcastDispatchPerMessage+4; i++ {
		// Distinct multi-rune names so a full squad exceeds the broadcast cap.
		roster = append(roster, TeamAgent{Name: "agent-" + string(rune('a'+i)), Status: "working"})
	}
	msg := agentMsg("all hands", "party", "", nil)
	targets, dropped, _ := resolveMentionTargets(msg, roster)
	if len(targets) != maxBroadcastDispatchPerMessage {
		t.Fatalf("broadcast dispatched %d, want cap of %d", len(targets), maxBroadcastDispatchPerMessage)
	}
	if dropped != 4 {
		t.Fatalf("broadcast dropped = %d, want 4 (non-silent truncation)", dropped)
	}
}

// --- ISI-5265: an explicit @-mention on a party post still targets only those named ---

func TestResolveExplicitMentionOnPartyDoesNotBroadcast(t *testing.T) {
	roster := []TeamAgent{
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
		{Name: "amy", Status: "working"},
	}
	// Human party post WITH an @-mention must target only the mentioned agent, never the whole room.
	msg := agentMsg("@john can you take this?", "party", "", nil)
	got := names(mustResolve(t, msg, roster))
	if len(got) != 1 || got[0] != "john" {
		t.Fatalf("explicit @-mention on party = %v, want only [john] (no broadcast)", got)
	}
}

func mustResolve(t *testing.T, msg *Message, roster []TeamAgent) []MentionDispatch {
	t.Helper()
	targets, _, _ := resolveMentionTargets(msg, roster)
	return targets
}

// --- (b) agent→agent loop is bounded (loop guard) ---------------------------

func TestResolveLoopGuardBoundsAgentChain(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "working"}}

	// A human turn (no agentID, no hop payload) → dispatches at hop 1.
	human := agentMsg("@john go", "party", "", nil)
	h1 := mustResolve(t, human, roster)
	if len(h1) != 1 || h1[0].HopDepth != 1 {
		t.Fatalf("human turn = %v, want one dispatch at hop 1", h1)
	}

	// john (agent) replies at hop 1 and @-mentions jane → dispatches jane at hop 2 (still within cap).
	hop1Payload := StampDispatchHop(nil, 1)
	agentHop1 := agentMsg("@jane your turn", "party", "john", &hop1Payload)
	h2 := mustResolve(t, agentHop1, roster)
	if len(h2) != 1 || h2[0].AgentName != "jane" || h2[0].HopDepth != 2 {
		t.Fatalf("hop-1 agent = %v, want jane at hop 2", h2)
	}

	// jane (agent) replies at hop 2 and @-mentions john → hop 3 would exceed the cap → nobody.
	hop2Payload := StampDispatchHop(nil, 2)
	agentHop2 := agentMsg("@john back to you", "party", "jane", &hop2Payload)
	if got := mustResolve(t, agentHop2, roster); len(got) != 0 {
		t.Fatalf("hop-2 agent = %v, want nobody (loop guard tripped)", names(got))
	}
}

// --- de-dupe: one dispatch per (agent) within a post ------------------------

func TestResolveDeDupesRepeatedMention(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}}
	msg := agentMsg("@john @john @John", "party", "", nil)
	if got := mustResolve(t, msg, roster); len(got) != 1 {
		t.Fatalf("de-dupe = %v, want a single dispatch for john", names(got))
	}
}

// --- opt-out: paused/blocked agents are not auto-dispatched ------------------

func TestResolveOptOutPausedAgent(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "paused"}, {Name: "jane", Status: "blocked"}, {Name: "amy", Status: "working"}}
	msg := agentMsg("@john @jane @amy", "party", "", nil)
	got := names(mustResolve(t, msg, roster))
	if len(got) != 1 || got[0] != "amy" {
		t.Fatalf("opt-out = %v, want only the working agent amy", got)
	}
}

// --- self-mention: an agent never auto-dispatches itself --------------------

func TestResolveSkipsSelfMention(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "working"}}
	// john (agent) @-mentions itself and jane; only jane is dispatched.
	msg := agentMsg("@john note to self, @jane please help", "party", "john", nil)
	got := names(mustResolve(t, msg, roster))
	if len(got) != 1 || got[0] != "jane" {
		t.Fatalf("self-mention = %v, want only jane", got)
	}
}

// --- ISI-5283: a large explicit @-mention set collapses to ONE coordinator dispatch ---
//
// Before ISI-5283 an @-mention post that named more agents than maxMentionDispatchPerMessage was
// capped and the tail dropped. Now a 2+-mention post routes to the Team Coordinator ONCE, so the
// per-message fan-out cap is never reached by explicit mentions — a post naming the whole squad is a
// single coordinator dispatch, no drops.
func TestResolveMultiMentionCollapsesToCoordinator(t *testing.T) {
	roster := []TeamAgent{{Name: "coord", Status: "working", Coordinator: true}}
	body := "@coord "
	for i := 0; i < maxMentionDispatchPerMessage+3; i++ {
		name := "agent-" + string(rune('a'+i))
		roster = append(roster, TeamAgent{Name: name, Status: "working"})
		body += "@" + name + " "
	}
	msg := agentMsg(body, "party", "", nil)
	targets, dropped, fallback := resolveMentionTargets(msg, roster)
	if fallback || dropped != 0 {
		t.Fatalf("fallback=%v dropped=%d, want a clean single coordinator dispatch", fallback, dropped)
	}
	if len(targets) != 1 || targets[0].AgentName != "coord" || !targets[0].Orchestrate {
		t.Fatalf("targets = %+v, want one orchestration dispatch to coord", targets)
	}
}

// --- dispatchMentions hook: wiring, provenance stamping, best-effort ---------

func TestDispatchMentionsHookStampsContextAndProvenance(t *testing.T) {
	roster := &fakeRoster{agents: []TeamAgent{{Name: "john", Status: "working"}}}
	disp := &fakeDispatcher{}
	h := NewHandlerWithDeps(nil, nil, roster)
	h.SetMentionDispatcher(disp)

	team := uuid.New()
	agentID := "pm-1"
	auth := AuthorContext{Principal: "agent:pm", TeamID: team, AgentID: &agentID}
	msg := agentMsg("@john please implement", "party", "pm-1", nil)

	h.dispatchMentions(context.Background(), "squad-a/proj", auth, msg)

	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher called %d times, want 1", len(disp.calls))
	}
	c := disp.calls[0]
	if c.AgentName != "john" || c.ProjectID != "squad-a/proj" || c.ThreadID != msg.ThreadID ||
		c.MessageID != msg.ID || c.TeamID != team {
		t.Fatalf("dispatch context = %+v, want thread/message/project/team stamped", c)
	}
	if c.TriggeredByPrincipal != "agent:pm" || c.TriggeredByAgentID == nil || *c.TriggeredByAgentID != "pm-1" {
		t.Fatalf("trigger provenance = %+v, want server-stamped agent identity", c)
	}
	if c.HopDepth != 1 {
		t.Fatalf("hop = %d, want 1 (fresh agent post carries no hop payload)", c.HopDepth)
	}
}

func TestDispatchMentionsHookBestEffort(t *testing.T) {
	roster := &fakeRoster{agents: []TeamAgent{{Name: "john", Status: "working"}}}
	disp := &fakeDispatcher{err: errors.New("run mint failed")}
	h := NewHandlerWithDeps(nil, nil, roster)
	h.SetMentionDispatcher(disp)

	// A dispatcher error must not panic or propagate — the message is already committed.
	h.dispatchMentions(context.Background(), "squad-a/proj",
		AuthorContext{Principal: "user:alice", TeamID: uuid.New()},
		agentMsg("@john hi", "party", "", nil))
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher called %d times, want 1 (error swallowed)", len(disp.calls))
	}
}

// TestDispatchMentionsFromBroadcastsHumanParty proves the SHARED edge both write paths call (REST
// Handler.postMessage and the MCP discussion_post tool) broadcasts a human bare party post to the
// whole roster — so the two edges cannot drift on ISI-5265 broadcast behaviour.
func TestDispatchMentionsFromBroadcastsHumanParty(t *testing.T) {
	roster := []TeamAgent{{Name: "john", Status: "working"}, {Name: "jane", Status: "idle"}}
	disp := &fakeDispatcher{}
	// Human-authored: AuthorContext carries no AgentID, msg carries no agentID → hop-0 broadcast.
	msg := agentMsg("whole squad sync", "party", "", nil)
	DispatchMentionsFrom(context.Background(), disp, roster, "squad-a/proj",
		AuthorContext{Principal: "user:alice", TeamID: uuid.New()}, msg)
	if len(disp.calls) != 2 {
		t.Fatalf("broadcast via shared edge dispatched %d, want 2 (whole roster)", len(disp.calls))
	}
}

func TestDispatchMentionsHookNoDispatcherNoop(t *testing.T) {
	roster := &fakeRoster{agents: []TeamAgent{{Name: "john", Status: "working"}}}
	h := NewHandlerWithDeps(nil, nil, roster) // no dispatcher wired
	// Must not panic and must dispatch nobody (room stays coordination-free).
	h.dispatchMentions(context.Background(), "squad-a/proj",
		AuthorContext{Principal: "user:alice", TeamID: uuid.New()},
		agentMsg("@john hi", "party", "", nil))
}

// ============================================================================
// ISI-5283 / ISI-5267 WS-2: multi-mention (2+) routing → Team Coordinator role
// ============================================================================

// Two resolvable mentions route to the Team Coordinator as a SINGLE orchestration dispatch carrying
// the structured directive (the mentioned agents + the verbatim request body), NOT one Run per agent.
func TestResolveMultiMentionRoutesToCoordinator(t *testing.T) {
	roster := []TeamAgent{
		{Name: "coord", Status: "idle", Coordinator: true},
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
	}
	msg := agentMsg("@john and @jane can you ship the login page?", "party", "", nil)
	targets, dropped, fallback := resolveMentionTargets(msg, roster)
	if fallback || dropped != 0 {
		t.Fatalf("fallback=%v dropped=%d, want a clean coordinator dispatch", fallback, dropped)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %v, want exactly ONE dispatch (to the coordinator)", names(targets))
	}
	got := targets[0]
	if got.AgentName != "coord" {
		t.Fatalf("dispatched %q, want the coordinator 'coord'", got.AgentName)
	}
	if !got.Orchestrate {
		t.Fatal("coordinator dispatch must set Orchestrate")
	}
	if got.HopDepth != 1 {
		t.Fatalf("hop = %d, want 1 (human turn)", got.HopDepth)
	}
	if len(got.OrchestratedAgents) != 2 || got.OrchestratedAgents[0] != "john" || got.OrchestratedAgents[1] != "jane" {
		t.Fatalf("OrchestratedAgents = %v, want [john jane] in first-seen order", got.OrchestratedAgents)
	}
	if got.RequestBody != msg.Body {
		t.Fatalf("RequestBody = %q, want the verbatim comment body", got.RequestBody)
	}
}

// The threshold is 2 RESOLVABLE mentions (D2): a post naming two agents where only one is on the
// roster stays an ordinary single-agent dispatch, never a coordinator hand-off.
func TestResolveTwoMentionsOnlyOneResolvableStaysDirect(t *testing.T) {
	roster := []TeamAgent{
		{Name: "coord", Status: "working", Coordinator: true},
		{Name: "john", Status: "working"},
	}
	msg := agentMsg("@john and @ghost please look", "party", "", nil)
	targets, _, fallback := resolveMentionTargets(msg, roster)
	if fallback {
		t.Fatal("one resolvable mention must not trip the multi-mention coordinator route")
	}
	if len(targets) != 1 || targets[0].AgentName != "john" || targets[0].Orchestrate {
		t.Fatalf("targets = %+v, want a single direct dispatch to john", targets)
	}
}

// D3 fallback: 2+ resolvable mentions on a Team with NO Coordinator role dispatch NOBODY (the caller
// surfaces a picker) — the engine never silently fans out to every mentioned agent.
func TestResolveMultiMentionNoCoordinatorFallsBack(t *testing.T) {
	roster := []TeamAgent{
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
	}
	msg := agentMsg("@john @jane split this up", "party", "", nil)
	targets, dropped, fallback := resolveMentionTargets(msg, roster)
	if !fallback {
		t.Fatal("2+ mentions with no coordinator must signal the D3 picker fallback")
	}
	if len(targets) != 0 || dropped != 0 {
		t.Fatalf("fallback must dispatch nobody, got targets=%v dropped=%d", names(targets), dropped)
	}
}

// A coordinator role that is opted out (paused/blocked) is NOT dispatchable, so a multi-mention post
// falls back to the picker rather than waking a paused coordinator or fanning out.
func TestResolveMultiMentionPausedCoordinatorFallsBack(t *testing.T) {
	roster := []TeamAgent{
		{Name: "coord", Status: "paused", Coordinator: true},
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
	}
	msg := agentMsg("@john @jane go", "party", "", nil)
	targets, _, fallback := resolveMentionTargets(msg, roster)
	if !fallback || len(targets) != 0 {
		t.Fatalf("paused coordinator must fall back: fallback=%v targets=%v", fallback, names(targets))
	}
}

// The coordinator itself authoring a 2+-mention post must not self-dispatch; it falls back to the
// picker instead of looping on itself.
func TestResolveMultiMentionByCoordinatorFallsBack(t *testing.T) {
	roster := []TeamAgent{
		{Name: "coord", Status: "working", Coordinator: true},
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
	}
	// Coordinator (agent-authored) @-mentions two teammates. selfAgent == coord → no self-dispatch.
	msg := agentMsg("@john @jane let's structure this", "party", "coord", nil)
	targets, _, fallback := resolveMentionTargets(msg, roster)
	if !fallback || len(targets) != 0 {
		t.Fatalf("coordinator self-authored multi-mention must fall back: fallback=%v targets=%v", fallback, names(targets))
	}
}

// A bare human party broadcast (no @-mention) is NOT a multi-mention post: it keeps the whole-room
// fan-out and is never collapsed to the coordinator, even when the roster has a coordinator role.
func TestResolveBroadcastNotRoutedToCoordinator(t *testing.T) {
	roster := []TeamAgent{
		{Name: "coord", Status: "working", Coordinator: true},
		{Name: "john", Status: "working"},
		{Name: "jane", Status: "working"},
	}
	msg := agentMsg("whole squad, standup now", "party", "", nil)
	targets, _, fallback := resolveMentionTargets(msg, roster)
	if fallback {
		t.Fatal("a broadcast must not trip the coordinator fallback")
	}
	if len(targets) != 3 {
		t.Fatalf("broadcast = %v, want all three agents (fan-out preserved)", names(targets))
	}
	for _, tg := range targets {
		if tg.Orchestrate {
			t.Fatalf("broadcast target %q must not be an orchestration dispatch", tg.AgentName)
		}
	}
}
