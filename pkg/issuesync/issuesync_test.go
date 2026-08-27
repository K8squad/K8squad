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

package issuesync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

// fakeProvider records outbound UpdateIssue calls; every other seam method
// is a stub (the engine only calls UpdateIssue).
type fakeProvider struct {
	name   string
	edits  []scm.IssueUpdate
	ids    []string
	failOn bool
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) Snapshot(_ context.Context, _ string, _ scm.SnapshotOptions) ([]scm.NormalizedRecord, error) {
	return nil, fmt.Errorf("unused in engine tests")
}
func (p *fakeProvider) VerifyWebhookDelivery(_ context.Context, _ http.Header, _ []byte, _ string) bool {
	return false
}
func (p *fakeProvider) ParseWebhookEvent(_ context.Context, _ http.Header, _ []byte) (*scm.WebhookEvent, error) {
	return nil, fmt.Errorf("unused")
}
func (p *fakeProvider) CreateComment(_ context.Context, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("unused")
}
func (p *fakeProvider) CreateStatus(_ context.Context, _, _ string, _ scm.Status) error {
	return fmt.Errorf("unused")
}
func (p *fakeProvider) GetRepo(_ context.Context, _ string) (*scm.Repository, error) {
	return nil, fmt.Errorf("unused")
}
func (p *fakeProvider) UpdateIssue(_ context.Context, _, externalID string, update scm.IssueUpdate) error {
	if p.failOn {
		return fmt.Errorf("provider write refused (test)")
	}
	p.edits = append(p.edits, update)
	p.ids = append(p.ids, externalID)
	return nil
}

// fakeStore is the in-memory Store double: links, one work item per test,
// and a record of every audit row and bookkeeping roll.
type fakeStore struct {
	links    []Link
	items    map[string]WorkItemSnapshot // by work item id
	inbound  []InboundApply
	outbound []OutboundApply
	observed []Observation
	audits   []auditRow
	raceOn   bool // force ErrLaneRace once
}

type auditRow struct {
	WorkItemID string
	From, To   string
	Payload    map[string]any
}

func (s *fakeStore) ListLinks(_ context.Context, _, _ string) ([]Link, error) {
	return s.links, nil
}

func (s *fakeStore) ReadWorkItem(_ context.Context, workItemID string) (WorkItemSnapshot, error) {
	item, ok := s.items[workItemID]
	if !ok {
		return WorkItemSnapshot{}, ErrNoSuchWorkItem
	}
	return item, nil
}

func (s *fakeStore) ApplyInbound(_ context.Context, link Link, apply InboundApply) error {
	if s.raceOn {
		s.raceOn = false
		return ErrLaneRace
	}
	if apply.ToState != apply.FromState {
		item := s.items[link.WorkItemID]
		apply.Obs = apply.Obs.withKSquad(item.UpdatedAt.Add(time.Second)) // post-write roll
		s.items[link.WorkItemID] = WorkItemSnapshot{State: apply.ToState, UpdatedAt: item.UpdatedAt.Add(time.Second)}
	}
	s.inbound = append(s.inbound, apply)
	s.audits = append(s.audits, decodeAudit(t_global, link.WorkItemID, apply.FromState, apply.ToState, apply.AuditPayload))
	s.rollLink(link.ID, apply.Obs)
	return nil
}

func (s *fakeStore) ApplyOutbound(_ context.Context, link Link, apply OutboundApply) error {
	s.outbound = append(s.outbound, apply)
	s.audits = append(s.audits, decodeAudit(t_global, link.WorkItemID, apply.FromState, apply.ToState, apply.AuditPayload))
	s.rollLink(link.ID, apply.Obs)
	return nil
}

func (s *fakeStore) Observe(_ context.Context, link Link, obs Observation) error {
	s.observed = append(s.observed, obs)
	s.rollLink(link.ID, obs)
	return nil
}

func (s *fakeStore) rollLink(id string, obs Observation) {
	for i := range s.links {
		if s.links[i].ID == id {
			s.links[i].ExternalState = obs.ExternalState
			s.links[i].ExternalLabels = obs.ExternalLabels
			s.links[i].ExternalUpdatedAt = obs.ExternalUpdatedAt
			s.links[i].KSquadUpdatedAt = obs.KSquadUpdatedAt
			s.links[i].LastWriter = obs.LastWriter
			s.links[i].Direction = obs.Direction
		}
	}
}

// t_global is the current test for decodeAudit error reporting (set in each
// test's prologue — a test helper cannot take *testing.T through the Store
// interface).
var t_global *testing.T

func decodeAudit(t *testing.T, workItemID, from, to string, payload json.RawMessage) auditRow {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("audit payload not JSON: %v", err)
	}
	return auditRow{WorkItemID: workItemID, From: from, To: to, Payload: fields}
}

// fixture assembles one link + one work item + one mirrored issue.
type fixture struct {
	store *fakeStore
	prov  *fakeProvider
	link  Link
}

func baseTime() time.Time { return time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC) }

func newFixture(t *testing.T, itemState string, issueState string, issueActor string) *fixture {
	t.Helper()
	t_global = t
	now := baseTime()
	f := &fixture{
		store: &fakeStore{items: map[string]WorkItemSnapshot{
			// The item was last touched an hour ago — aligned with the link
			// baselines below, so a fresh mirror observation is NOT a
			// conflict by default (tests that want one move the item).
			"wi-1": {State: itemState, UpdatedAt: now.Add(-time.Hour)},
		}},
		prov: &fakeProvider{name: "github"},
		link: Link{
			ID:                "link-1",
			ProjectNamespace:  "ns",
			ProjectName:       "proj",
			WorkItemID:        "wi-1",
			Provider:          "github",
			Repo:              "github.com/acme/app",
			ExternalID:        "42",
			Direction:         DirectionInbound,
			Provenance:        ProvenanceExternalSourced,
			LastWriter:        WriterExternal,
			ExternalState:     issueState,
			ExternalLabels:    []string{},
			ExternalUpdatedAt: now.Add(-time.Hour), // last observed an hour ago
			KSquadUpdatedAt:   now.Add(-time.Hour),
		},
	}
	f.store.links = []Link{f.link}
	return f
}

// mirror builds the pass's mirror input for the fixture's issue.
func (f *fixture) mirrorRow(state string, labels []string, updatedAt time.Time, actor string) []scm.MirrorRow {
	payload, _ := json.Marshal(scm.MirrorPayload{Labels: labels, UpdatedAt: updatedAt})
	return []scm.MirrorRow{{
		ProjectNamespace: "ns",
		ProjectName:      "proj",
		Kind:             scm.RecordTypeIssue,
		ExternalID:       "42",
		State:            state,
		Title:            "issue 42",
		Actor:            actor,
		Trust:            scm.TrustUntrustedExternal,
		Payload:          payload,
	}}
}

func (f *fixture) sync(ctx context.Context, t *testing.T, direction string, rows []scm.MirrorRow) SyncStats {
	t.Helper()
	stats, err := NewSyncer(f.store).SyncProject(ctx, "ns", "proj", f.prov, "github.com/acme/app", direction, rows)
	if err != nil {
		t.Fatalf("SyncProject: %v", err)
	}
	return stats
}

// AC1 inbound: a closed GitHub issue moves the linked work item to done,
// with an audit row recording the apply.
func TestInboundClosedIssueMovesItemToDone(t *testing.T) {
	f := newFixture(t, "in_progress", "open", "dev")
	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateClosed, []string{"bug"}, baseTime(), "dev"))

	if stats.InboundApplied != 1 {
		t.Fatalf("InboundApplied = %d, want 1", stats.InboundApplied)
	}
	item := f.store.items["wi-1"]
	if item.State != "done" {
		t.Fatalf("item state = %q, want done", item.State)
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(f.store.audits))
	}
	audit := f.store.audits[0]
	if audit.From != "in_progress" || audit.To != "done" {
		t.Fatalf("audit from/to = %q/%q, want in_progress/done", audit.From, audit.To)
	}
	if audit.Payload["winner"] != WriterExternal {
		t.Fatalf("audit winner = %v, want external", audit.Payload["winner"])
	}
	if audit.Payload["conflict"] != false {
		t.Fatalf("audit conflict = %v, want false", audit.Payload["conflict"])
	}
	if link := f.store.links[0]; link.LastWriter != WriterExternal {
		t.Fatalf("link last_writer = %q, want external", link.LastWriter)
	}
}

// AC1 inbound reopen: a reopened issue pulls a done item back to todo.
func TestInboundReopenPullsDoneItemBack(t *testing.T) {
	f := newFixture(t, "done", "closed", "dev")
	f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, nil, baseTime(), "dev"))
	if item := f.store.items["wi-1"]; item.State != "todo" {
		t.Fatalf("item state = %q, want todo (reopen)", item.State)
	}
}

// An open issue never commands a lane: GitHub cannot express lanes, so a
// live item keeps its lane while the labels still flow.
func TestInboundOpenIssueKeepsLiveLane(t *testing.T) {
	f := newFixture(t, "in_progress", "open", "dev")
	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, []string{"prio"}, baseTime(), "dev"))
	if item := f.store.items["wi-1"]; item.State != "in_progress" {
		t.Fatalf("item state = %q, want in_progress (no forced lane)", item.State)
	}
	if stats.InboundApplied != 1 {
		t.Fatalf("InboundApplied = %d, want 1 (label apply)", stats.InboundApplied)
	}
	if link := f.store.links[0]; len(link.ExternalLabels) != 1 || link.ExternalLabels[0] != "prio" {
		t.Fatalf("labels = %v, want [prio]", link.ExternalLabels)
	}
}

// Label-only change applies labels without a lane write.
func TestInboundLabelOnlyApply(t *testing.T) {
	f := newFixture(t, "in_progress", "open", "dev")
	f.link.ExternalLabels = []string{"old"}
	f.store.links[0].ExternalLabels = []string{"old"}
	f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, []string{"new"}, baseTime(), "dev"))
	if len(f.store.inbound) != 1 {
		t.Fatalf("applies = %d, want 1", len(f.store.inbound))
	}
	if ap := f.store.inbound[0]; ap.FromState != "in_progress" || ap.ToState != "in_progress" {
		t.Fatalf("label-only apply wrote a lane: %q -> %q", ap.FromState, ap.ToState)
	}
}

// Convergence (OQ13): an already-agreeing, label-equal pass writes nothing
// but bookkeeping.
func TestConvergentPassIsObserveOnly(t *testing.T) {
	f := newFixture(t, "todo", "open", "dev")
	now := baseTime()
	f.store.links[0].ExternalUpdatedAt = now
	f.store.links[0].KSquadUpdatedAt = now
	f.store.links[0].ExternalLabels = []string{"bug"}

	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, []string{"bug"}, now, "dev"))

	if stats.InboundApplied != 0 || stats.OutboundApplied != 0 || stats.ConflictsResolved != 0 {
		t.Fatalf("convergent pass applied something: %+v", stats)
	}
	if len(f.store.observed) != 1 {
		t.Fatalf("observations = %d, want 1", len(f.store.observed))
	}
}

// Echo suppression (OQ13): a mirror row authored by the bot identity is our
// own reflected write and is skipped entirely.
func TestEchoSuppressionSkipsBotAuthoredRows(t *testing.T) {
	f := newFixture(t, "in_progress", "open", scm.DefaultBotActor)
	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateClosed, nil, baseTime(), scm.DefaultBotActor))
	if stats.EchoSuppressed != 1 || stats.InboundApplied != 0 {
		t.Fatalf("stats = %+v, want echo-suppressed no-op", stats)
	}
	if item := f.store.items["wi-1"]; item.State != "in_progress" {
		t.Fatalf("bot-authored close leaked into the item: %q", item.State)
	}
}

// An issue that vanished from the snapshot (deleted upstream) is counted,
// not fatal.
func TestMissingMirrorRowCounts(t *testing.T) {
	f := newFixture(t, "todo", "open", "dev")
	stats := f.sync(context.Background(), t, DirectionInbound, nil)
	if stats.MissingMirror != 1 || stats.InboundApplied != 0 {
		t.Fatalf("stats = %+v, want missing-mirror no-op", stats)
	}
}

// AC1 vice-versa: with direction=bidirectional and the KSquad write newer,
// a lane change reflects back through the seam as an issue state edit.
func TestOutboundBidirectionalReflectsLaneChange(t *testing.T) {
	f := newFixture(t, "done", "open", "dev")
	now := baseTime()
	f.store.items["wi-1"] = WorkItemSnapshot{State: "done", UpdatedAt: now}
	f.store.links[0].ExternalUpdatedAt = now.Add(-time.Hour)
	f.store.links[0].KSquadUpdatedAt = now.Add(-time.Hour)

	stats := f.sync(context.Background(), t, DirectionBidirectional,
		f.mirrorRow(scm.IssueStateOpen, nil, now.Add(-time.Hour), "dev"))

	if stats.OutboundApplied != 1 {
		t.Fatalf("OutboundApplied = %d, want 1", stats.OutboundApplied)
	}
	if len(f.prov.edits) != 1 {
		t.Fatalf("provider edits = %d, want 1", len(f.prov.edits))
	}
	if f.prov.edits[0].State != scm.IssueStateClosed {
		t.Fatalf("commanded state = %q, want closed (done lane)", f.prov.edits[0].State)
	}
	if link := f.store.links[0]; link.LastWriter != WriterKSquad || link.ExternalState != scm.IssueStateClosed {
		t.Fatalf("link bookkeeping = %v/%v, want ksquad/closed", link.LastWriter, link.ExternalState)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Payload["winner"] != WriterKSquad {
		t.Fatalf("outbound audit missing or wrong winner: %+v", f.store.audits)
	}
}

// AC3 conflict, ksquad wins: both sides changed since the last pass and the
// KSquad write is newer — the outbound apply overrides the external change
// and the audit row records a resolved conflict.
func TestOutboundConflictKsquadWinsAudited(t *testing.T) {
	f := newFixture(t, "done", "closed", "dev")
	now := baseTime()
	// Both baselines 3h old; the external edit landed 1h ago (pending) and
	// the KSquad lane move landed just now (pending, NEWER): conflict,
	// ksquad wins, the outbound apply commands closed (done lane) and the
	// audit records the conflict over the pending external edit.
	f.store.items["wi-1"] = WorkItemSnapshot{State: "done", UpdatedAt: now}
	f.store.links[0].ExternalUpdatedAt = now.Add(-3 * time.Hour)
	f.store.links[0].KSquadUpdatedAt = now.Add(-3 * time.Hour)

	stats := f.sync(context.Background(), t, DirectionBidirectional,
		f.mirrorRow(scm.IssueStateOpen, nil, now.Add(-time.Hour), "dev"))

	if stats.ConflictsResolved != 1 {
		t.Fatalf("ConflictsResolved = %d, want 1", stats.ConflictsResolved)
	}
	if f.prov.edits[0].State != scm.IssueStateClosed {
		t.Fatalf("commanded = %q, want closed (ksquad lane wins)", f.prov.edits[0].State)
	}
	if f.store.audits[0].Payload["conflict"] != true {
		t.Fatalf("audit conflict = %v, want true", f.store.audits[0].Payload["conflict"])
	}
}

// Direction gating: with direction=inbound, a newer KSquad lane change
// stands (LWW) but never reflects out — absorbed, no provider call, no
// audit row.
func TestInboundDirectionAbsorbsNewerKSquadChange(t *testing.T) {
	f := newFixture(t, "done", "open", "dev")
	now := baseTime()
	f.store.items["wi-1"] = WorkItemSnapshot{State: "done", UpdatedAt: now}
	f.store.links[0].ExternalUpdatedAt = now.Add(-time.Hour)
	f.store.links[0].KSquadUpdatedAt = now.Add(-time.Hour)

	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, nil, now.Add(-time.Hour), "dev"))

	if stats.OutboundApplied != 0 || len(f.prov.edits) != 0 {
		t.Fatalf("inbound direction reflected out: %+v %v", stats, f.prov.edits)
	}
	if len(f.store.audits) != 0 {
		t.Fatalf("absorb wrote an audit row: %+v", f.store.audits)
	}
	if link := f.store.links[0]; link.LastWriter != WriterKSquad {
		t.Fatalf("absorb did not roll last_writer: %q", link.LastWriter)
	}
}

// A provider failure on the outbound path surfaces as an error and leaves
// the audit trail empty (level-triggered retry on the next pass).
func TestOutboundProviderFailureSurfaces(t *testing.T) {
	f := newFixture(t, "done", "open", "dev")
	f.prov.failOn = true
	now := baseTime()
	f.store.items["wi-1"] = WorkItemSnapshot{State: "done", UpdatedAt: now}
	f.store.links[0].ExternalUpdatedAt = now.Add(-time.Hour)
	f.store.links[0].KSquadUpdatedAt = now.Add(-time.Hour)

	if _, err := NewSyncer(f.store).SyncProject(context.Background(), "ns", "proj", f.prov,
		"github.com/acme/app", DirectionBidirectional,
		f.mirrorRow(scm.IssueStateOpen, nil, now.Add(-time.Hour), "dev")); err == nil {
		t.Fatal("provider failure swallowed")
	}
	if len(f.store.audits) != 0 {
		t.Fatalf("failed outbound still audited: %+v", f.store.audits)
	}
}

// A lane race (the CAS guard missed) surfaces as ErrLaneRace.
func TestLaneRaceSurfaces(t *testing.T) {
	f := newFixture(t, "todo", "closed", "dev")
	f.store.raceOn = true
	if _, err := NewSyncer(f.store).SyncProject(context.Background(), "ns", "proj", f.prov,
		"github.com/acme/app", DirectionInbound,
		f.mirrorRow(scm.IssueStateClosed, nil, baseTime(), "dev")); err == nil {
		t.Fatal("lane race swallowed")
	}
}

// A missing work item is a hard error (the link is dangling — deleted item
// cascades the link away, so this is corruption, not a normal state).
func TestMissingWorkItemErrors(t *testing.T) {
	f := newFixture(t, "todo", "closed", "dev")
	delete(f.store.items, "wi-1")
	if _, err := NewSyncer(f.store).SyncProject(context.Background(), "ns", "proj", f.prov,
		"github.com/acme/app", DirectionInbound,
		f.mirrorRow(scm.IssueStateClosed, nil, baseTime(), "dev")); err == nil {
		t.Fatal("missing work item swallowed")
	}
}

// No links configured: the pass is a free no-op.
func TestNoLinksIsNoOp(t *testing.T) {
	f := newFixture(t, "todo", "open", "dev")
	f.store.links = nil
	stats := f.sync(context.Background(), t, DirectionInbound,
		f.mirrorRow(scm.IssueStateOpen, nil, baseTime(), "dev"))
	if stats.Links != 0 {
		t.Fatalf("stats = %+v, want zero links", stats)
	}
}

// The state projection maps: closed⇄done, open pulls done back to todo,
// open leaves live lanes alone; unmapped states never move a lane.
func TestStateProjections(t *testing.T) {
	cases := []struct {
		external, current, wantLane string
	}{
		{"closed", "todo", "done"},
		{"closed", "in_progress", "done"},
		{"closed", "done", "done"},
		{"open", "done", "todo"},
		{"open", "in_progress", "in_progress"},
		{"open", "backlog", "backlog"},
		{"merged", "todo", "todo"}, // unmapped: never moves a lane
	}
	for _, c := range cases {
		if got := LaneForExternalState(c.external, c.current); got != c.wantLane {
			t.Errorf("LaneForExternalState(%q,%q) = %q, want %q", c.external, c.current, got, c.wantLane)
		}
	}
	lanes := []struct{ lane, want string }{
		{"done", scm.IssueStateClosed},
		{"todo", scm.IssueStateOpen},
		{"in_progress", scm.IssueStateOpen},
		{"in_review", scm.IssueStateOpen},
		{"backlog", scm.IssueStateOpen},
	}
	for _, c := range lanes {
		if got := ExternalStateForLane(c.lane); got != c.want {
			t.Errorf("ExternalStateForLane(%q) = %q, want %q", c.lane, got, c.want)
		}
	}
}

// Idempotence of the full loop: apply → converge → re-apply the SAME
// snapshot writes nothing the second time (AC2 discipline).
func TestSecondPassIsConvergent(t *testing.T) {
	f := newFixture(t, "in_progress", "open", "dev")
	rows := f.mirrorRow(scm.IssueStateClosed, []string{"bug"}, baseTime(), "dev")
	f.sync(context.Background(), t, DirectionInbound, rows)

	applies := len(f.store.inbound)
	audits := len(f.store.audits)
	stats := f.sync(context.Background(), t, DirectionInbound, rows)

	if stats.InboundApplied != 0 {
		t.Fatalf("second pass re-applied: %+v", stats)
	}
	if len(f.store.inbound) != applies || len(f.store.audits) != audits {
		t.Fatalf("second pass mutated applies/audits: %d/%d -> %d/%d",
			applies, audits, len(f.store.inbound), len(f.store.audits))
	}
}
