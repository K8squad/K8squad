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

// Package issuesync hosts the story-11.2 issue⇄work-item sync engine
// (arch §5.4 / FR-H1, ISI-2738).
//
// The repo-sync reconciler (story 11.1) already converges the untrusted
// scm mirror from a provider snapshot on every level-triggered pass. This
// package consumes that SAME snapshot — never a webhook payload — and, for
// every issue link in scm.issue_link, drives status and labels across the
// seam:
//
//   - inbound (always, LWW): the mirrored issue's open/closed projection
//     maps onto the work item (closed → done; reopened → todo; an open
//     issue never forces a lane — GitHub cannot express lanes), and the
//     issue's labels flow onto the link as the item's external label set;
//   - outbound (only when the Project's configured direction is
//     bidirectional): a KSquad lane change reflects back through the
//     SourceProvider seam as an issue state edit, origin-marked upstream
//     by the bot credential for echo suppression.
//
// Conflict policy (AC3, §6.5): last-writer-wins by write timestamp — the
// provider-side updated_at vs the work item's updated_at, each diffed
// against the link's rolling bookkeeping. A win over a LIVE loser (both
// sides changed since the last pass) is a CONFLICT: the apply writes an
// audit row naming the winner, the loser and both timestamps, in the same
// transaction as the change itself. Non-conflicting applies are audited
// too (event_type='issue_sync') — provenance is not an error path here.
//
// Convergence (OQ13 discipline): applies are decided by CONTENT — the two
// state projections and the label set — so a pass that finds both sides
// already agreeing writes nothing but bookkeeping. Our own inbound write
// rolls the link's ksquad bookkeeping to the post-write updated_at, and
// our own outbound write converges the projections, so neither can echo
// back as a phantom fresh change beyond one benign observe-only pass.
package issuesync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

// Direction values mirrored from the CRD enum (RepoIssueSyncSpec).
const (
	DirectionInbound       = "inbound"
	DirectionBidirectional = "bidirectional"
)

// Provenance values (AC2): who authored the linked item FIRST. Set once at
// link creation, immutable thereafter — the console badges items from it.
const (
	ProvenanceKSquadNative    = "ksquad-native"
	ProvenanceExternalSourced = "external-sourced"
)

// Last-writer identities for the LWW bookkeeping (AC3).
const (
	WriterExternal = "external"
	WriterKSquad   = "ksquad"
)

// SyncPrincipal is the coord.audit_log principal stamped on sync-driven
// writes — a reserved system identity, never a human or agent principal.
const SyncPrincipal = "scm-issue-sync"

// Board lanes the engine commands (the 0001 work_item.state enum subset
// the open/closed projection can express).
const (
	laneDone = "done"
	laneTodo = "todo"
)

// Link is the Go view of one scm.issue_link row (story 11.2 AC2): the
// KSquad-owned half of the bijection, with the rolling LWW bookkeeping.
type Link struct {
	ID                string
	ProjectNamespace  string
	ProjectName       string
	WorkItemID        string
	Provider          string
	Repo              string
	ExternalID        string
	ExternalURL       string
	Direction         string
	Provenance        string
	LastWriter        string
	ExternalState     string
	ExternalLabels    []string
	ExternalUpdatedAt time.Time
	KSquadUpdatedAt   time.Time
	LastSyncedAt      time.Time
}

// WorkItemSnapshot is the coordination-side state one pass reads.
type WorkItemSnapshot struct {
	State     string
	UpdatedAt time.Time
}

// SyncStats reports one pass's outcome (observation, not control input).
type SyncStats struct {
	Links             int // links examined
	InboundApplied    int // work-item state/label applies from the provider side
	OutboundApplied   int // provider issue edits from the KSquad side
	ConflictsResolved int // applies that overrode a live loser (audited as conflicts)
	EchoSuppressed    int // mirror rows authored by the bot identity, skipped
	MissingMirror     int // links whose issue vanished from the snapshot (deleted upstream)
}

// Store is the persistence seam for the engine: link bookkeeping, the
// fenced work-item lane, and §6.5 audit rows. SQLStore (store.go) is the
// production implementation over the shared coordination Postgres.
type Store interface {
	// ListLinks returns the project's issue links.
	ListLinks(ctx context.Context, projectNamespace, projectName string) ([]Link, error)

	// ReadWorkItem returns the item's current lane and updated_at.
	ReadWorkItem(ctx context.Context, workItemID string) (WorkItemSnapshot, error)

	// ApplyInbound atomically CASes the work item's lane (label-only
	// applies pass FromState==ToState), writes the audit row and rolls the
	// link bookkeeping forward. ErrLaneRace means the lane moved under the
	// apply — nothing was written; the next pass re-decides.
	ApplyInbound(ctx context.Context, link Link, apply InboundApply) error

	// ApplyOutbound records an ALREADY-PERFORMED provider write (audit row
	// + link bookkeeping; no work-item write). Called only after
	// SourceProvider.UpdateIssue succeeded.
	ApplyOutbound(ctx context.Context, link Link, apply OutboundApply) error

	// Observe rolls the link bookkeeping forward without applying anything
	// (a convergent no-op pass still refreshes the LWW baseline).
	Observe(ctx context.Context, link Link, obs Observation) error
}

// InboundApply is one external-wins apply.
type InboundApply struct {
	// FromState/ToState are the work item's lane before/after. Equal on a
	// label-only apply (no lane write, audit still written).
	FromState, ToState string

	// Conflict marks an LWW win over a live KSquad change (AC3).
	Conflict bool

	// AuditPayload is the §6.5 provenance record for the apply.
	AuditPayload json.RawMessage

	// Obs is the post-apply bookkeeping.
	Obs Observation
}

// OutboundApply is one ksquad-wins apply recorded after the provider call.
type OutboundApply struct {
	// FromState/ToState are the work item's lane (unchanged by an outbound
	// apply — recorded for the audit row).
	FromState, ToState string

	// CommandedState is the normalized external state written upstream.
	CommandedState string

	// Conflict marks an LWW win over a live external change (AC3).
	Conflict bool

	AuditPayload json.RawMessage

	Obs Observation
}

// Observation is the link bookkeeping one pass leaves behind: the rolling
// LWW baseline both sides are diffed against on the NEXT pass.
type Observation struct {
	ExternalState     string
	ExternalLabels    []string
	ExternalUpdatedAt time.Time
	KSquadUpdatedAt   time.Time
	LastWriter        string
	Direction         string
}

// ErrLaneRace is returned by ApplyInbound when the work item's lane moved
// between the pass's read and its apply (the CAS guard bit). Nothing was
// written; level-triggered retry re-decides on the next pass.
var ErrLaneRace = fmt.Errorf("issuesync: work item lane changed under apply")

// Syncer is the story-11.2 engine: one level-triggered pass over a
// project's issue links per repo-sync reconcile. It holds no mutable Go
// state; every method opens its own store round trips.
type Syncer struct {
	Store Store

	// BotActor is the echo-suppression identity (default
	// scm.DefaultBotActor): mirror rows authored by the bot are our own
	// reflected writes and are skipped (OQ13).
	BotActor string
}

// NewSyncer builds a Syncer over the store.
func NewSyncer(store Store) *Syncer {
	return &Syncer{Store: store}
}

func (s *Syncer) botActor() string {
	if s.BotActor == "" {
		return scm.DefaultBotActor
	}
	return s.BotActor
}

// SyncProject drives one level-triggered pass for the project's issue
// links, diffing the just-applied mirror snapshot (issue rows only) against
// the link bookkeeping and the work items' current lanes. provider is the
// SAME seam instance the reconciler snapshotted through; repoURL and
// direction are the Project's configured values (spec.repo.url and
// spec.repo.sync.issueSync.direction, default inbound).
func (s *Syncer) SyncProject(ctx context.Context, projectNamespace, projectName string, provider scm.SourceProvider, repoURL, direction string, rows []scm.MirrorRow) (SyncStats, error) {
	links, err := s.Store.ListLinks(ctx, projectNamespace, projectName)
	if err != nil {
		return SyncStats{}, fmt.Errorf("issuesync: list links %s/%s: %w", projectNamespace, projectName, err)
	}
	if len(links) == 0 {
		return SyncStats{}, nil // no linkage configured: the pass is a no-op
	}

	issues := map[string]scm.MirrorRow{}
	for _, row := range rows {
		if row.Kind == scm.RecordTypeIssue {
			issues[row.ExternalID] = row
		}
	}

	stats := SyncStats{}
	for _, link := range links {
		st, err := s.syncOne(ctx, link, provider, repoURL, direction, issues)
		if err != nil {
			return stats, fmt.Errorf("issuesync: link %s %s#%s: %w", link.Provider, link.Repo, link.ExternalID, err)
		}
		stats.merge(st)
	}
	return stats, nil
}

func (st *SyncStats) merge(o SyncStats) {
	st.Links++
	st.InboundApplied += o.InboundApplied
	st.OutboundApplied += o.OutboundApplied
	st.ConflictsResolved += o.ConflictsResolved
	st.EchoSuppressed += o.EchoSuppressed
	st.MissingMirror += o.MissingMirror
}

// syncOne reconciles ONE link. Every decision is content-based (the two
// state projections + labels) with timestamps only breaking ties (LWW).
func (s *Syncer) syncOne(ctx context.Context, link Link, provider scm.SourceProvider, repoURL, direction string, issues map[string]scm.MirrorRow) (SyncStats, error) {
	var stats SyncStats

	row, ok := issues[link.ExternalID]
	if !ok {
		// The snapshot no longer contains the issue: deleted (or made
		// invisible to the mirror token) upstream. The mirror converged it
		// away; the link stays for provenance/audit. Nothing to sync.
		stats.MissingMirror = 1
		return stats, nil
	}

	// Echo suppression (OQ13): an issue record authored by the bot identity
	// is our own reflected write re-observed; applying it back would loop.
	if row.Actor == s.botActor() {
		stats.EchoSuppressed = 1
		return stats, nil
	}

	var payload scm.MirrorPayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return stats, fmt.Errorf("parse mirror payload: %w", err)
		}
	}

	item, err := s.Store.ReadWorkItem(ctx, link.WorkItemID)
	if err != nil {
		return stats, fmt.Errorf("read work item: %w", err)
	}

	externalState := row.State // normalized open|closed behind the seam
	itemProjection := ExternalStateForLane(item.State)
	labelsChanged := !equalLabels(link.ExternalLabels, payload.Labels)
	projectionsAgree := externalState == itemProjection

	externalPending := payload.UpdatedAt.After(link.ExternalUpdatedAt)
	ksquadPending := item.UpdatedAt.After(link.KSquadUpdatedAt)
	conflict := externalPending && ksquadPending && !projectionsAgree

	// LWW: the newer write wins; a tie (or neither pending — drift after a
	// config flip, or the first pass after link creation) biases external,
	// the inbound default.
	winner := WriterExternal
	if ksquadPending && item.UpdatedAt.After(payload.UpdatedAt) {
		winner = WriterKSquad
	}

	obs := Observation{
		ExternalState:     externalState,
		ExternalLabels:    payload.Labels,
		ExternalUpdatedAt: payload.UpdatedAt,
		KSquadUpdatedAt:   item.UpdatedAt,
		LastWriter:        link.LastWriter,
		Direction:         direction,
	}

	switch {
	case !projectionsAgree && winner == WriterKSquad && direction == DirectionBidirectional:
		// Outbound apply (AC1 vice-versa half): reflect the lane change to
		// the provider issue through the seam. The provider call happens
		// BEFORE any bookkeeping write — a failed edit leaves the link
		// untouched and the next pass retries (level-triggered).
		commanded := ExternalStateForLane(item.State)
		if err := provider.UpdateIssue(ctx, repoURL, link.ExternalID, scm.IssueUpdate{State: commanded}); err != nil {
			return stats, fmt.Errorf("outbound reflect: %w", err)
		}
		obs.LastWriter = WriterKSquad
		obs.ExternalState = commanded
		audit, err := auditPayload(map[string]any{
			"initiator":     "issue-sync",
			"direction":     direction,
			"winner":        WriterKSquad,
			"conflict":      conflict,
			"from_state":    item.State,
			"to_state":      item.State,
			"external_from": externalState,
			"external_to":   commanded,
			"provider":      link.Provider,
			"repo":          link.Repo,
			"external_id":   link.ExternalID,
		})
		if err != nil {
			return stats, err
		}
		if err := s.Store.ApplyOutbound(ctx, link, OutboundApply{
			FromState:      item.State,
			ToState:        item.State,
			CommandedState: commanded,
			Conflict:       conflict,
			AuditPayload:   audit,
			Obs:            obs,
		}); err != nil {
			return stats, fmt.Errorf("record outbound: %w", err)
		}
		stats.OutboundApplied = 1
		if conflict {
			stats.ConflictsResolved = 1
		}
		return stats, nil

	case !projectionsAgree && winner == WriterKSquad:
		// KSquad wrote last but the direction is inbound-only: the local
		// change stands by configuration; absorb the bookkeeping so the
		// next pass does not re-litigate it. No audit row — this is
		// configured behaviour, not a resolved conflict.
		obs.LastWriter = WriterKSquad
		if err := s.Store.Observe(ctx, link, obs); err != nil {
			return stats, fmt.Errorf("absorb ksquad-lww: %w", err)
		}
		return stats, nil

	default:
		// External wins (or the projections already agree): inbound apply.
		// A disagreeing projection maps the issue state onto the lane; a
		// label-only change applies the labels. An agreeing, equal-label
		// pass is a pure observation.
		toState := item.State
		if !projectionsAgree {
			toState = LaneForExternalState(externalState, item.State)
		}
		if projectionsAgree && !labelsChanged {
			if err := s.Store.Observe(ctx, link, obs); err != nil {
				return stats, fmt.Errorf("observe: %w", err)
			}
			return stats, nil
		}
		obs.LastWriter = WriterExternal
		audit, err := auditPayload(map[string]any{
			"initiator":   "issue-sync",
			"direction":   direction,
			"winner":      WriterExternal,
			"conflict":    conflict,
			"from_state":  item.State,
			"to_state":    toState,
			"external_to": externalState,
			"labels":      payload.Labels,
			"provider":    link.Provider,
			"repo":        link.Repo,
			"external_id": link.ExternalID,
		})
		if err != nil {
			return stats, err
		}
		if err := s.Store.ApplyInbound(ctx, link, InboundApply{
			FromState:    item.State,
			ToState:      toState,
			Conflict:     conflict,
			AuditPayload: audit,
			Obs:          obs,
		}); err != nil {
			return stats, fmt.Errorf("apply inbound: %w", err)
		}
		stats.InboundApplied = 1
		if conflict {
			stats.ConflictsResolved = 1
		}
		return stats, nil
	}
}

// LaneForExternalState maps the normalized external issue state onto a
// board lane (the inbound half): closed → done; open on a done item → todo
// (reopen); open on a live lane keeps the lane — an open issue cannot
// command a lane because the provider cannot express lanes.
func LaneForExternalState(externalState, currentLane string) string {
	switch externalState {
	case scm.IssueStateClosed:
		return laneDone
	case scm.IssueStateOpen:
		if currentLane == laneDone {
			return laneTodo // reopened upstream: pull the item back to the board
		}
		return currentLane
	default:
		// An unmapped provider state is never allowed to move a lane; the
		// link's labels still flow (the default arm applies labels only).
		return currentLane
	}
}

// ExternalStateForLane projects a board lane onto the normalized external
// issue state (the outbound half): done → closed, every live lane → open.
func ExternalStateForLane(lane string) string {
	if lane == laneDone {
		return scm.IssueStateClosed
	}
	return scm.IssueStateOpen
}

// auditPayload renders the §6.5 audit payload deterministically (sorted
// keys are not required by the schema, but a stable rendering makes the
// audit trail diffable).
func auditPayload(fields map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("issuesync: audit payload: %w", err)
	}
	return json.RawMessage(b), nil
}

// equalLabels compares two label sets order-insensitively.
func equalLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
