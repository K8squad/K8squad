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

type fakeSettler struct {
	got  []settleCall
	fail bool
}

type settleCall struct {
	a2aTaskID, runID, outcome, reason string
}

func (f *fakeSettler) Settle(_ context.Context, a2aTaskID, runID, outcome, reason string) error {
	f.got = append(f.got, settleCall{a2aTaskID: a2aTaskID, runID: runID, outcome: outcome, reason: reason})
	if f.fail {
		return errors.New("settle: db down")
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

// ISI-7542: a Submit-failing lap older than FailTTL is settled follow_error —
// the durable marker that stops UnsettledDispatches from ever returning it
// again, routes the Run through the settled-non-success death path (ISI-7524),
// and lets the S2 reaper own the pod.
func TestReattachFollows_SettlesCorpseAfterTTL(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse", DispatchedAt: old},
		{A2ATaskID: "live", RunID: "run-live", DispatchedAt: old},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"corpse": errors.New("dial tcp: no route to host")}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler, FailTTL: time.Hour}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(settler.got) != 1 {
		t.Fatalf("settles = %+v, want exactly the corpse lap", settler.got)
	}
	c := settler.got[0]
	if c.a2aTaskID != "corpse" || c.runID != "run-corpse" {
		t.Fatalf("settled lap = %+v, want the corpse", c)
	}
	if c.outcome != coord.SettleOutcomeFollowError {
		t.Fatalf("outcome = %q, want %q (transport-level end, agent outcome unknown)", c.outcome, coord.SettleOutcomeFollowError)
	}
	if c.reason == "" {
		t.Fatal("settle reason must carry the submit error")
	}
	// The live lap is untouched by the corpse's settlement.
	if len(disp.got) != 2 {
		t.Fatalf("both laps must be attempted, got %+v", disp.got)
	}
}

// A Submit-failing lap YOUNGER than FailTTL stays unsettled: a transient
// failure may recover on a later leader-elect, and the corpse TTL only bounds
// how long the retry may go on.
func TestReattachFollows_YoungFailureStaysUnsettled(t *testing.T) {
	young := time.Now().Add(-5 * time.Minute)
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "blip", RunID: "run-blip", DispatchedAt: young},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"blip": errors.New("dial tcp: connection refused")}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler, FailTTL: time.Hour}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(settler.got) != 0 {
		t.Fatalf("young failure must NOT settle, got %+v", settler.got)
	}
}

// A zero DispatchedAt (a reader that does not populate the dispatch instant)
// can never prove lap age, so it must keep the legacy log-and-skip behavior.
func TestReattachFollows_ZeroDispatchedAtNeverSettles(t *testing.T) {
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "opaque", RunID: "run-opaque"}, // DispatchedAt zero
	}}
	disp := &fakeReattacher{failOn: map[string]error{"opaque": errors.New("boom")}}
	settler := &fakeSettler{}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler, FailTTL: time.Minute}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(settler.got) != 0 {
		t.Fatalf("unprovable age must NOT settle, got %+v", settler.got)
	}
}

// A settle-write error fails open toward retry: the lap stays unsettled (next
// leader-elect re-lists it), the pass continues, and Start stays best-effort.
func TestReattachFollows_SettleErrorLeavesUnsettledAndContinues(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour)
	reader := &fakeReattachReader{targets: []coord.ReattachTarget{
		{A2ATaskID: "corpse", RunID: "run-corpse", DispatchedAt: old},
		{A2ATaskID: "live", RunID: "run-live", DispatchedAt: old},
	}}
	disp := &fakeReattacher{failOn: map[string]error{"corpse": errors.New("no route to host")}}
	settler := &fakeSettler{fail: true}
	r := &ReattachFollows{Reader: reader, Dispatcher: disp, Settler: settler, FailTTL: time.Hour}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start must stay best-effort, got: %v", err)
	}
	if len(settler.got) != 1 {
		t.Fatalf("settle must be attempted once, got %+v", settler.got)
	}
	if len(disp.got) != 2 {
		t.Fatalf("both laps must still be attempted, got %+v", disp.got)
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
