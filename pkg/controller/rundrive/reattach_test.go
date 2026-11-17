/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rundrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

type fakeReattachReader struct {
	targets   []coord.ReattachTarget
	err       error
	gotWindow time.Duration
	calls     int
}

func (f *fakeReattachReader) UnsettledDispatches(_ context.Context, window time.Duration) ([]coord.ReattachTarget, error) {
	f.calls++
	f.gotWindow = window
	return f.targets, f.err
}

type fakeReattacher struct {
	got    []coord.ReattachTarget
	failOn map[string]error // a2aTaskID -> error to return
}

func (f *fakeReattacher) Submit(_ context.Context, a2aTaskID, runID string) error {
	f.got = append(f.got, coord.ReattachTarget{A2ATaskID: a2aTaskID, RunID: runID})
	if e, ok := f.failOn[a2aTaskID]; ok {
		return e
	}
	return nil
}

// Every listed lap is re-submitted, in order, with the (a2aTaskID, runID) the
// reader returned.
func TestReattachFollows_ReattachesEveryLap(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "run-a#lap2", RunID: "run-a"},
		{A2ATaskID: "run-b", RunID: "run-b"},
	}}
	disp := &fakeReattacher{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Window: 3 * time.Hour}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(disp.got) != 2 || disp.got[0].A2ATaskID != "run-a#lap2" || disp.got[1].RunID != "run-b" {
		t.Fatalf("re-attach set = %+v, want both laps in order", disp.got)
	}
	if reader.gotWindow != 3*time.Hour {
		t.Fatalf("reader window = %s, want 3h", reader.gotWindow)
	}
}

// A supervisor-gone Submit error on one lap is logged and skipped; the
// remaining laps still re-attach (best-effort, per §5 option a).
func TestReattachFollows_SupervisorGoneSkipsAndContinues(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "gone", RunID: "run-gone"},
		{A2ATaskID: "live", RunID: "run-live"},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"gone": errors.New("submit: supervisor unreachable")}}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start must be best-effort (nil), got: %v", err)
	}
	// Both were attempted; the live one is not blocked by the gone one.
	if len(disp.got) != 2 {
		t.Fatalf("both laps must be attempted, got %+v", disp.got)
	}
}

// A nil window falls back to DefaultReattachWindow.
func TestReattachFollows_DefaultWindow(t *testing.T) {
	reader := &fakeReattachReader{}
	r := &ReattachFollows{Reader: reader, Dispatcher: &fakeReattacher{}}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if reader.gotWindow != DefaultReattachWindow {
		t.Fatalf("window = %s, want default %s", reader.gotWindow, DefaultReattachWindow)
	}
}

// A reader error aborts the pass but never the operator: Start returns nil and
// nothing is re-submitted.
func TestReattachFollows_ReaderErrorIsBestEffort(t *testing.T) {
	reader := &fakeReattachReader{err: errors.New("db down")}
	disp := &fakeReattacher{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("reader error must not fail the operator, got: %v", err)
	}
	if len(disp.got) != 0 {
		t.Fatalf("a reader error must re-attach nothing, got %+v", disp.got)
	}
}

// A nil dispatcher (ledger-only operator) makes Start an inert no-op: the reader
// is never even consulted.
func TestReattachFollows_NilDispatcherIsNoOp(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{{A2ATaskID: "x", RunID: "x"}}}
	r := &ReattachFollows{Reader: reader, Dispatcher: nil}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if reader.calls != 0 {
		t.Fatalf("nil dispatcher must not query the reader, calls=%d", reader.calls)
	}
}

// A context already canceled before the loop stops the pass cleanly without
// re-submitting stale laps.
func TestReattachFollows_CanceledContextStops(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "a", RunID: "a"},
		{A2ATaskID: "b", RunID: "b"},
	}}
	disp := &fakeReattacher{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(disp.got) != 0 {
		t.Fatalf("a canceled context must re-attach nothing, got %+v", disp.got)
	}
}

type settledCall struct {
	a2aTaskID, runID, outcome, reason string
}

type fakeSettler struct {
	gotCalls []settledCall
	err      error
}

func (f *fakeSettler) Settle(_ context.Context, a2aTaskID, runID, outcome, reason string) error {
	f.gotCalls = append(f.gotCalls, settledCall{a2aTaskID: a2aTaskID, runID: runID, outcome: outcome, reason: reason})
	return f.err
}

// ISI-7540: a Submit-failing lap older than FailTTL is terminally settled
// follow_error through the coord seam — it can never settle on its own (dead
// sandbox), so leaving it unsettled would re-Submit it on every leader-elect
// for the whole window. A young failing lap is left alone (a transient blip
// gets the next elect), and a succeeding lap is never settled here (its OnDone
// owns the settlement).
func TestReattachFollows_StaleFailingLapSettledFollowError(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour)
	young := time.Now().Add(-5 * time.Minute)
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse", DispatchedAt: stale},
		{A2ATaskID: "blip", RunID: "run-blip", DispatchedAt: young},
		{A2ATaskID: "oldlive", RunID: "run-oldlive", DispatchedAt: stale},
	}}
	disp := &fakeReattacher{failOn: map[string]error{
		"corpse": errors.New("submit: dial tcp: no route to host"),
		"blip":   errors.New("submit: transient"),
	}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler} // FailTTL zero -> default 2h

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(disp.got) != 3 {
		t.Fatalf("all laps must be attempted, got %+v", disp.got)
	}
	if len(settler.gotCalls) != 1 {
		t.Fatalf("exactly the stale failing lap must settle, got %+v", settler.gotCalls)
	}
	c := settler.gotCalls[0]
	if c.a2aTaskID != "corpse" || c.runID != "run-corpse" {
		t.Fatalf("settled lap = %s/%s, want corpse/run-corpse", c.a2aTaskID, c.runID)
	}
	if c.outcome != coord.SettleOutcomeFollowError {
		t.Fatalf("outcome = %q, want %q", c.outcome, coord.SettleOutcomeFollowError)
	}
	if !strings.Contains(c.reason, "no route to host") {
		t.Fatalf("reason must carry the Submit error, got %q", c.reason)
	}
}

// A custom FailTTL is honored: a 30m-old failing lap settles under FailTTL=10m
// and would not under the 2h default.
func TestReattachFollows_FailTTLFieldHonored(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "x", RunID: "run-x", DispatchedAt: time.Now().Add(-30 * time.Minute)},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"x": errors.New("gone")}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler, FailTTL: 10 * time.Minute}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(settler.gotCalls) != 1 {
		t.Fatalf("lap older than custom FailTTL must settle, got %+v", settler.gotCalls)
	}
}

// A settler error never breaks the pass: Start stays nil and later laps still
// re-attach (the next leader-elect retries the settle).
func TestReattachFollows_SettlerErrorIsBestEffort(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour)
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse", DispatchedAt: stale},
		{A2ATaskID: "live", RunID: "run-live", DispatchedAt: stale},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"corpse": errors.New("gone")}}
	settler := &fakeSettler{err: errors.New("db down")}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("settler error must not fail the pass, got: %v", err)
	}
	if len(disp.got) != 2 {
		t.Fatalf("both laps must still be attempted, got %+v", disp.got)
	}
}

// A nil Settler keeps the legacy behaviour: a stale failing lap is only logged
// and left to age out of the window.
func TestReattachFollows_NilSettlerKeepsLegacyBehaviour(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse", DispatchedAt: time.Now().Add(-3 * time.Hour)},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"corpse": errors.New("gone")}}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("lap must be attempted, got %+v", disp.got)
	}
}

// A zero DispatchedAt (reader that cannot age the lap) must never trigger the
// TTL settle: time.Since(zero) dwarfs any TTL, so without the guard a single
// Submit failure would settle the lap immediately (cherry-picked from the
// duplicate-lane review of #1049).
func TestReattachFollows_ZeroDispatchedAtNeverSettles(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse"}, // zero DispatchedAt
	}}
	disp := &fakeReattacher{failOn: map[string]error{"corpse": errors.New("gone")}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(disp.got) != 1 {
		t.Fatalf("lap must be attempted, got %+v", disp.got)
	}
	if len(settler.gotCalls) != 0 {
		t.Fatalf("zero DispatchedAt must never settle, got %+v", settler.gotCalls)
	}
}
