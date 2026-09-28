package mentiondispatch

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// ErrProjectNotFound is returned when a project reference resolves to no Project CR (existence-hiding —
// the dispatcher treats it as best-effort "no dispatch", never a crash). ErrProjectAmbiguous is a
// bare-name that collides across squads (the caller should address it by "namespace/name" or UID).
var (
	ErrProjectNotFound  = errors.New("project not found")
	ErrProjectAmbiguous = errors.New("project name is ambiguous across squads")
)

// clientProjectResolver resolves a project reference through a controller-runtime client.Reader (an
// informer cache) — the cmd/memory analogue of apiserver's clientProjectRefResolver, so the memory
// service resolves a Project identically to the console. It shares the Team-CR cache reader the assign
// verb already uses (NewTeamCacheReader), which lists Project CRs from the same ksquad scheme.
type clientProjectResolver struct {
	reader client.Reader
}

// NewClientProjectResolver binds a ProjectResolver to a Project/Team-reading client.Reader (the shared
// informer cache). Used by cmd/memory to give the discussion_post dispatch path slug→UID resolution
// without importing internal/apiserver (which would be an import cycle via internal/memory).
func NewClientProjectResolver(reader client.Reader) ProjectResolver {
	return clientProjectResolver{reader: reader}
}

// ResolveProject matches, in priority order (UID-first so a bare-name collision can never shadow a UID):
//  1. Project CR UID (unique ⇒ wins immediately);
//  2. "namespace/name" composite — the canonical console id;
//  3. bare Project name — ErrProjectAmbiguous on a cross-squad collision.
//
// No match ⇒ ErrProjectNotFound. Mirrors apiserver.clientProjectRefResolver.ResolveProjectRef exactly so
// both processes key coord rows on the same Project/Team UIDs.
func (r clientProjectResolver) ResolveProject(ctx context.Context, projectRef string) (ResolvedProject, error) {
	if projectRef == "" {
		return ResolvedProject{}, ErrProjectNotFound
	}
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects); err != nil {
		return ResolvedProject{}, err
	}
	var match *ksquadv1.Project
	nameMatches := 0
	for i := range projects.Items {
		p := &projects.Items[i]
		if string(p.UID) == projectRef {
			match = p
			nameMatches = 1
			break
		}
		if p.Namespace+"/"+p.Name == projectRef {
			match = p
			nameMatches = 1
			break // composite ids are unique by construction (namespace-scoped name)
		}
		if p.Name == projectRef {
			match = p
			nameMatches++
		}
	}
	switch {
	case nameMatches == 0 || match == nil:
		return ResolvedProject{}, ErrProjectNotFound
	case nameMatches > 1:
		return ResolvedProject{}, ErrProjectAmbiguous
	}
	out := ResolvedProject{UID: string(match.UID)}
	team, err := teamInNamespace(ctx, r.reader, match.Namespace)
	if err != nil {
		return ResolvedProject{}, err
	}
	if team != nil {
		out.TeamUID = string(team.UID)
	}
	return out, nil
}

// teamInNamespace returns the Team CR whose namespace is ns, or (nil, nil) when none is found. Mirrors
// apiserver's helper of the same name — the Team home namespace is the squad the project's agents live in.
func teamInNamespace(ctx context.Context, reader client.Reader, ns string) (*ksquadv1.Team, error) {
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return &teams.Items[i], nil
		}
	}
	return nil, nil
}
