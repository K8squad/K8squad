package discussion

// Party-mode budget + opt-in gating coverage (ISI-5585, WS-B, ADR-0027 §5). The budget policy and the
// opt-in guard are PURE (no I/O), so these ride the default unit lane with no database — exactly like
// the sibling resolveMentionTargets tests. The store CRUD (StartPartySession / AdvanceRound /
// RecordPaidRuns / CloseSession) is a DB concern exercised by the migration self-check
// (0033_discussion_party_session_test.sql) and the apiserver integration lane (WS-D).

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Budget defaults + normalization (§5.1)
// ---------------------------------------------------------------------------

func TestDefaultPartyBudget_DerivesHardCeiling(t *testing.T) {
	b := DefaultPartyBudget()
	if b.MaxRounds != DefaultMaxPartyRounds {
		t.Fatalf("MaxRounds = %d, want %d", b.MaxRounds, DefaultMaxPartyRounds)
	}
	if b.MaxVoicesPerRound != DefaultVoicesPerRound {
		t.Fatalf("MaxVoicesPerRound = %d, want %d", b.MaxVoicesPerRound, DefaultVoicesPerRound)
	}
	// Ceiling = rounds×voices + rounds facilitator runs = 3×3 + 3 = 12 (ADR-0027 §5.1 worst case).
	if want := DefaultMaxPartyRounds*DefaultVoicesPerRound + DefaultMaxPartyRounds; b.PaidRunBudget != want {
		t.Fatalf("derived PaidRunBudget = %d, want %d", b.PaidRunBudget, want)
	}
}

func TestPartyBudget_Normalize(t *testing.T) {
	cases := []struct {
		name      string
		in        PartyBudget
		wantErr   error
		wantMaxVo int
		// wantCeil<=0 means "don't assert the exact ceiling" (only the explicit-override case pins it)
		wantCeil int
	}{
		{name: "zero fills defaults + derives ceiling", in: PartyBudget{}, wantMaxVo: DefaultVoicesPerRound, wantCeil: 12},
		{name: "voices below band clamp up to 2", in: PartyBudget{MaxRounds: 2, MaxVoicesPerRound: 1}, wantMaxVo: MinVoicesPerRound},
		{name: "voices above band clamp down to 4", in: PartyBudget{MaxRounds: 2, MaxVoicesPerRound: 9}, wantMaxVo: MaxVoicesPerRound},
		{name: "explicit ceiling honoured", in: PartyBudget{MaxRounds: 3, MaxVoicesPerRound: 3, PaidRunBudget: 5}, wantMaxVo: 3, wantCeil: 5},
		{name: "negative round rejected", in: PartyBudget{MaxRounds: -1}, wantErr: ErrInvalidPartyBudget},
		{name: "negative ceiling rejected", in: PartyBudget{PaidRunBudget: -3}, wantErr: ErrInvalidPartyBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.normalize()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.MaxVoicesPerRound != tc.wantMaxVo {
				t.Fatalf("MaxVoicesPerRound = %d, want %d", got.MaxVoicesPerRound, tc.wantMaxVo)
			}
			if tc.wantCeil > 0 && got.PaidRunBudget != tc.wantCeil {
				t.Fatalf("PaidRunBudget = %d, want %d", got.PaidRunBudget, tc.wantCeil)
			}
			if got.PaidRunBudget <= 0 {
				t.Fatalf("PaidRunBudget must be positive after normalize, got %d", got.PaidRunBudget)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CanStartRound — the hard gate that replaces the hop-cap inside a session (§3.2/§5.1)
// ---------------------------------------------------------------------------

func TestPartySession_CanStartRound(t *testing.T) {
	budget := PartyBudget{MaxRounds: 3, MaxVoicesPerRound: 3, PaidRunBudget: 12}
	cases := []struct {
		name       string
		phase      string
		round      int
		used       int
		wantOK     bool
		wantReason string
	}{
		{name: "fresh active session can start round 1", phase: PartyPhaseActive, round: 0, used: 0, wantOK: true},
		{name: "mid-session with headroom can advance", phase: PartyPhaseActive, round: 1, used: 4, wantOK: true},
		{name: "round cap reached closes", phase: PartyPhaseActive, round: 3, used: 6, wantOK: false, wantReason: PartyPhaseClosed},
		{name: "paid-run ceiling reached exhausts (precedes round check)", phase: PartyPhaseActive, round: 1, used: 12, wantOK: false, wantReason: PartyPhaseBudgetExhausted},
		{name: "no headroom for even the facilitator mint", phase: PartyPhaseActive, round: 0, used: 12, wantOK: false, wantReason: PartyPhaseBudgetExhausted},
		{name: "already closed never starts", phase: PartyPhaseClosed, round: 1, used: 2, wantOK: false, wantReason: PartyPhaseClosed},
		{name: "already budget_exhausted never starts", phase: PartyPhaseBudgetExhausted, round: 1, used: 12, wantOK: false, wantReason: PartyPhaseBudgetExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := PartySession{Phase: tc.phase, Round: tc.round, PaidRunsUsed: tc.used, Budget: budget}
			ok, reason := s.CanStartRound()
			if ok != tc.wantOK {
				t.Fatalf("CanStartRound ok = %v, want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if !ok && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// VoicesAllowedThisRound — per-round fan-out cap AND paid-run headroom (§5.1), non-silent capping
// ---------------------------------------------------------------------------

func TestPartySession_VoicesAllowedThisRound(t *testing.T) {
	budget := PartyBudget{MaxRounds: 3, MaxVoicesPerRound: 3, PaidRunBudget: 12}
	cases := []struct {
		name        string
		used        int
		requested   int
		wantAllowed int
		wantCapped  int
	}{
		{name: "request under cap passes through", used: 0, requested: 2, wantAllowed: 2, wantCapped: 0},
		{name: "request over per-round cap is capped to 3", used: 0, requested: 5, wantAllowed: 3, wantCapped: 2},
		{name: "headroom tighter than cap wins", used: 10, requested: 3, wantAllowed: 2, wantCapped: 1},
		{name: "no headroom dispatches nobody", used: 12, requested: 3, wantAllowed: 0, wantCapped: 3},
		{name: "zero request dispatches nobody", used: 0, requested: 0, wantAllowed: 0, wantCapped: 0},
		{name: "negative request dispatches nobody", used: 0, requested: -2, wantAllowed: 0, wantCapped: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := PartySession{Phase: PartyPhaseActive, PaidRunsUsed: tc.used, Budget: budget}
			allowed, capped := s.VoicesAllowedThisRound(tc.requested)
			if allowed != tc.wantAllowed || capped != tc.wantCapped {
				t.Fatalf("VoicesAllowedThisRound(%d) with used=%d = (allowed %d, capped %d), want (%d, %d)",
					tc.requested, tc.used, allowed, capped, tc.wantAllowed, tc.wantCapped)
			}
			// Invariant: allowed + capped always re-accounts the (non-negative) request — never a
			// silent drop (ADR-0027 §5.1).
			req := tc.requested
			if req < 0 {
				req = 0
			}
			if allowed+capped != req {
				t.Fatalf("allowed(%d)+capped(%d) != request(%d) — a drop was swallowed", allowed, capped, req)
			}
		})
	}
}

func TestPartySession_RemainingPaidRuns(t *testing.T) {
	s := PartySession{Budget: PartyBudget{PaidRunBudget: 12}, PaidRunsUsed: 5}
	if got := s.RemainingPaidRuns(); got != 7 {
		t.Fatalf("RemainingPaidRuns = %d, want 7", got)
	}
	// Over-spend never goes negative (the ceiling is hard).
	s.PaidRunsUsed = 15
	if got := s.RemainingPaidRuns(); got != 0 {
		t.Fatalf("RemainingPaidRuns over budget = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Opt-in gating — only a human may open a session (§5.2)
// ---------------------------------------------------------------------------

func TestPartyStartAllowed_HumanOnly(t *testing.T) {
	if err := PartyStartAllowed(AuthorContext{Principal: "henrik"}); err != nil {
		t.Fatalf("human opener rejected: %v", err)
	}
	agentID := "coordinator"
	err := PartyStartAllowed(AuthorContext{Principal: "coordinator", AgentID: &agentID})
	if !errors.Is(err, ErrPartyStartForbidden) {
		t.Fatalf("agent opener err = %v, want ErrPartyStartForbidden", err)
	}
}
