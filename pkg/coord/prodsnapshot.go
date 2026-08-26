// prodsnapshot.go adapts the git-native build-browser reader (internal/buildbrowser) to the coord
// BuildSnapshotter seam ProdEffects.WithSnapshotter consumes. It is the production capture wiring for
// the 8.7c build-snapshot: bound to one Run's worktree + refs, it produces the content-addressed uri
// + sha256 + summary meta that Collect persists as the kind="build-snapshot" artifact.
package coord

import (
	"context"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

// buildbrowserSnapshotter binds a buildbrowser.GitReader to one Run's workspace and refs.
type buildbrowserSnapshotter struct {
	reader *buildbrowser.GitReader
	meta   buildbrowser.RunMeta
}

// NewBuildbrowserSnapshotter binds the git-native 8.7c capture to a Run's workspace. repoPath is the
// Run's worktree; headRef/baseRef are the run/base refs the snapshot is taken between (the §9.4
// worktree model: run branch vs Project default ref). The capture is read-only on the worktree.
func NewBuildbrowserSnapshotter(repoPath, runID, headRef, baseRef string) BuildSnapshotter {
	return &buildbrowserSnapshotter{
		reader: buildbrowser.NewGitReader(),
		meta: buildbrowser.RunMeta{
			RunID:    runID,
			RepoPath: repoPath,
			HeadRef:  headRef,
			BaseRef:  baseRef,
		},
	}
}

// Snapshot captures the Run's build and projects the buildbrowser summary onto the coord meta object
// (base, runRef, commit, fileCount, totalAdditions, totalDeletions, truncated).
func (s *buildbrowserSnapshotter) Snapshot(ctx context.Context) (BuildSnapshot, error) {
	snap, err := s.reader.Snapshot(ctx, s.meta)
	if err != nil {
		return BuildSnapshot{}, err
	}
	return BuildSnapshot{
		URI:    snap.URI,
		SHA256: snap.SHA256,
		Meta: map[string]any{
			"base":           snap.Summary.Base,
			"runRef":         snap.Summary.RunRef,
			"commit":         snap.Summary.Commit,
			"fileCount":      snap.Summary.FileCount,
			"totalAdditions": snap.Summary.TotalAdditions,
			"totalDeletions": snap.Summary.TotalDeletions,
			"truncated":      snap.Summary.Truncated,
		},
	}, nil
}
