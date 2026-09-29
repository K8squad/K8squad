package apiserver

import (
	"context"
	"errors"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Discussion ticket-picker project narrow — slug → Project CR UID resolver (ISI-5213).
// ============================================================================
//
// The discussion @-mention ticket picker searches the work-item corpus (pkg/search) and then narrows
// the hits to THIS room's project. The search read model returns each hit's ProjectID as the Project
// CR UID (coord.work_item.project_id, w.project_id::text), but the {projectId} route carries the
// console's canonical "namespace/name" slug (ISI-3982). Comparing the slug against the UID directly
// dropped EVERY row for any query — the picker returned nothing (ISI-5213), the same slug↔UID mismatch
// class ISI-5170 solved for the ticket-reference resolver.
//
// This adapts the apiserver's shared ProjectRefResolver — the SAME informer-cache seam the dispatch,
// ticket-reference, and room-tenancy resolvers ride — onto the neutral discussion.ProjectUIDResolver
// seam, so the picker resolves a Project's UID identically to its sibling surfaces (no second slug→UID
// mapping is introduced). Wire the result with (*discussion.Handler).SetProjectUIDResolver.

// NewProjectUIDResolver builds the discussion ticket-picker project-narrow resolver over the shared
// project-ref resolver. A nil refs (cache-less dev host) returns nil, so SetProjectUIDResolver(nil)
// leaves the picker narrowing against the raw {projectId} path value exactly as before.
func NewProjectUIDResolver(refs ProjectRefResolver) discussion.ProjectUIDResolver {
	if refs == nil {
		return nil
	}
	return projectUIDResolver{refs: refs}
}

type projectUIDResolver struct{ refs ProjectRefResolver }

// ResolveProjectUID maps the room's {projectId} slug to the owning Project CR UID. ok=false (never an
// error) when the Project cannot be resolved — unknown or ambiguous (a bare-name cross-squad collision)
// — so the handler falls back to the raw-path narrow rather than dropping every hit. A genuine infra
// error (informer cache read failure) is surfaced so the handler can fall back rather than silently
// mis-scope.
func (r projectUIDResolver) ResolveProjectUID(ctx context.Context, projectID string) (string, bool, error) {
	res, err := r.refs.ResolveProjectRef(ctx, projectID)
	if err != nil {
		if errors.Is(err, ErrProjectNotFound) || errors.Is(err, ErrProjectAmbiguous) {
			return "", false, nil
		}
		return "", false, err
	}
	if res.UID == "" {
		return "", false, nil
	}
	return res.UID, true, nil
}
