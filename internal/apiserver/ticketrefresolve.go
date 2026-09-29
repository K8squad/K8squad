package apiserver

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// Discussion ticket-reference resolution — apiserver wiring (ISI-5165 seam / ISI-5170).
// ============================================================================
//
// ISI-5165 (PR #692) landed the discussion-layer contract — the discussion.TicketRefResolver seam,
// the TicketRef / StampReferences / ReferencesOf payload helpers, normalizeTicketRefs, and the
// postMessage merge hook — but left the resolver nil, so references were dropped and no link ever
// persisted (nil-safe, no regression). This file is the concrete, coord-backed implementation, wired
// in cmd/apiserver/main.go alongside SetMentionDispatcher / SetReplyHopResolver — the DB-backed half
// the ISI-5165 unit tests fake out (fakeRefResolver).
//
// The seam hands us the room's projectID (a "namespace/name" slug, ISI-3982), the caller's Team scope,
// and candidate work-item UUIDs (already deduped + UUID-validated by normalizeTicketRefs). A reference
// is a LINK, never a dispatch: we keep only the candidates that EXIST and belong to the SAME project as
// the room, within the caller's Team — an out-of-project or foreign-team UUID is dropped (existence-
// hiding), never persisted. Two coord facts resolve that:
//  1. the room's project UID — ProjectRefResolver maps the slug → Project CR UID (the same seam the
//     board read models use; coord.work_item.project_id keys on that UID, not the console slug);
//  2. each candidate's owning project + authoritative title — coord.WorkItemProject, team-scoped so a
//     cross-tenant probe reads as not-found.
//
// The surviving title is canonicalized from coord so a client-supplied chip label cannot spoof it.

// workItemProjectReader is the coord read the resolver needs: one work item's owning project + title,
// team-scoped. *coord.WorkItemReadStore satisfies it.
type workItemProjectReader interface {
	WorkItemProject(ctx context.Context, workItemID, teamID string) (coord.WorkItemProjectRef, error)
}

// ticketRefResolver is the coord-backed discussion.TicketRefResolver.
type ticketRefResolver struct {
	reads workItemProjectReader
	refs  ProjectRefResolver
}

// NewTicketRefResolver builds the coord-backed reference resolver. Wire it with
// (*discussion.Handler).SetTicketRefResolver. A nil coord read store or project resolver returns a nil
// interface (references stay dropped, posts stay link-free) — the same degrade-to-link-free contract the
// discussion seam documents for a nil resolver, so a DB-less dev run behaves exactly as before.
func NewTicketRefResolver(reads *coord.WorkItemReadStore, refs ProjectRefResolver) discussion.TicketRefResolver {
	if reads == nil || refs == nil {
		return nil
	}
	return ticketRefResolver{reads: reads, refs: refs}
}

// ResolveTicketRefs keeps only the candidates that exist and belong to the room's project within the
// caller's Team scope, canonicalizing each surviving Title from coord. Candidates are pre-normalized by
// the discussion layer (deduped, UUID-validated), so every WorkItemID here is a parseable UUID.
func (t ticketRefResolver) ResolveTicketRefs(ctx context.Context, projectID string, teamID uuid.UUID, refs []discussion.TicketRef) ([]discussion.TicketRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	// The room's project as a Project CR UID (coord.work_item.project_id keys on this, not the slug).
	// A room whose project no longer resolves has no valid references — degrade to link-free (the
	// seam's best-effort contract) rather than fail the post.
	room, err := t.refs.ResolveProjectRef(ctx, projectID)
	if err != nil {
		return nil, nil
	}

	// teamID zero ⇒ the fleet-admin trusted path (mirrors coord's empty-teamID convention and the
	// AllTeams branch the mention search takes for admins); a scoped Team fences the read to its tenancy.
	var teamScope string
	if teamID != uuid.Nil {
		teamScope = teamID.String()
	}

	out := make([]discussion.TicketRef, 0, len(refs))
	for _, ref := range refs {
		item, err := t.reads.WorkItemProject(ctx, ref.WorkItemID, teamScope)
		if errors.Is(err, coord.ErrWorkItemNotFound) {
			continue // unknown id or outside the caller's Team — drop (existence-hiding)
		}
		if err != nil {
			return nil, err // a real read failure is the coord plane failing — surface it (whole set drops)
		}
		if item.ProjectID != room.UID {
			continue // out-of-project reference — a link may only target THIS room's project
		}
		title := ref.Title
		if item.Title != "" {
			title = item.Title // canonicalize the chip label from the authoritative source
		}
		out = append(out, discussion.TicketRef{WorkItemID: ref.WorkItemID, Title: title})
	}
	return out, nil
}
