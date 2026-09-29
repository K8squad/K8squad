package apiserver

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Discussion room tenancy — project's owning Team resolver (ISI-5198).
// ============================================================================
//
// The discussion handler fences every room operation on the room's OWNING Team (the Team that owns the
// Project's namespace), not the caller's session Team, so a Run scoped to the project's Team can post
// into a thread a fleet-admin opened (the ISI-5152 reply loop). This adapts the apiserver's shared
// ProjectRefResolver — the same informer-cache seam the dispatch + ticket-reference resolvers ride, so
// the room resolves a Project's Team identically to its sibling surfaces — onto the neutral
// discussion.ProjectTeamResolver seam. Wire the result with (*discussion.Handler).SetProjectTeamResolver.

// NewProjectTeamResolver builds the discussion room-tenancy resolver over the project-ref resolver. A
// nil refs (cache-less dev host) returns nil, so SetProjectTeamResolver(nil) leaves the room on the
// legacy caller-Team scope exactly as before.
func NewProjectTeamResolver(refs ProjectRefResolver) discussion.ProjectTeamResolver {
	if refs == nil {
		return nil
	}
	return projectTeamResolver{refs: refs}
}

type projectTeamResolver struct{ refs ProjectRefResolver }

// ResolveProjectTeam maps the room's {projectId} to the UID of the Team that owns the Project's
// namespace. ok=false (never an error) when the Project resolves but no Team claims its namespace
// (a partially-composed squad) or the ref cannot be resolved at all (unknown/ambiguous) — the handler
// then falls back to the caller's own Team scope. A genuine infra error (cache read failure) is
// returned so the handler can fall back rather than silently mis-scope.
func (r projectTeamResolver) ResolveProjectTeam(ctx context.Context, projectID string) (uuid.UUID, bool, error) {
	res, err := r.refs.ResolveProjectRef(ctx, projectID)
	if err != nil {
		if errors.Is(err, ErrProjectNotFound) || errors.Is(err, ErrProjectAmbiguous) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	if res.TeamUID == "" {
		return uuid.Nil, false, nil
	}
	teamID, perr := uuid.Parse(res.TeamUID)
	if perr != nil {
		return uuid.Nil, false, nil
	}
	return teamID, true, nil
}
