package apiserver

import (
	"context"
	"database/sql"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/mentiondispatch"
)

// ============================================================================
// Dispatch-on-mention run-minting — apiserver wiring (ISI-5116; hoisted in ISI-5125).
// ============================================================================
//
// The run-minting implementation of discussion.MentionDispatcher now lives in internal/mentiondispatch so
// BOTH the apiserver REST Handler and the cmd/memory discussion_post MCP tool drive one implementation and
// cannot drift (ISI-5125) — cmd/memory could not import internal/apiserver (that would be a cycle via
// internal/apiserver → internal/memory). This file keeps the apiserver-shaped constructors so cmd/apiserver
// wiring (SetMentionDispatcher / SetReplyHopResolver) is unchanged; they adapt the apiserver seams onto the
// neutral package's.

// NewMentionDispatcher builds the run-minting implementation of discussion.MentionDispatcher wired to the
// SAME coord seams the human board create/dispatch handlers ride (WorkItemWriter, WorkItemDispatcher) plus
// the project-ref resolver and the Postgres dispatch ledger. WorkItemWriter/WorkItemDispatcher satisfy the
// neutral package's narrower WorkItemCreator/WorkItemDispatcher structurally; the project resolver is
// adapted below. Wire the result with (*discussion.Handler).SetMentionDispatcher.
func NewMentionDispatcher(create WorkItemWriter, dispatch WorkItemDispatcher, refs ProjectRefResolver, db *sql.DB) discussion.MentionDispatcher {
	return mentiondispatch.New(create, dispatch, projectResolverAdapter{refs}, db)
}

// NewReplyHopResolver builds the discussion.ReplyHopResolver over the dispatch ledger so the discussion
// handler can stamp an agent reply's loop-guard hop from the Run's identity. Wire it with
// (*discussion.Handler).SetReplyHopResolver.
func NewReplyHopResolver(db *sql.DB) discussion.ReplyHopResolver {
	return mentiondispatch.NewReplyHopResolver(db)
}

// projectResolverAdapter adapts the apiserver ProjectRefResolver (console informer cache) onto the neutral
// mentiondispatch.ProjectResolver seam, mapping ProjectRefResolution → ResolvedProject. It keeps the
// apiserver's own resolver type untouched.
type projectResolverAdapter struct{ refs ProjectRefResolver }

func (a projectResolverAdapter) ResolveProject(ctx context.Context, projectRef string) (mentiondispatch.ResolvedProject, error) {
	r, err := a.refs.ResolveProjectRef(ctx, projectRef)
	if err != nil {
		return mentiondispatch.ResolvedProject{}, err
	}
	return mentiondispatch.ResolvedProject{UID: r.UID, TeamUID: r.TeamUID}, nil
}
