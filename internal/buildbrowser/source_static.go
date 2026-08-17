package buildbrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
)

// StaticRunSource is an in-memory RunSource for dev/test — the build-browser analogue of the
// apiserver's StaticSessionResolver. Production wires a Postgres-backed source (a Run's Team, owner,
// and workspace path come from the coord store); until then a JSON file lets a dev run drive the
// real read-model against a local repo. It is NOT for production.
type StaticRunSource struct {
	runs map[string]RunMeta
}

// Lookup implements RunSource.
func (s *StaticRunSource) Lookup(_ context.Context, runID string) (RunMeta, bool, error) {
	m, ok := s.runs[runID]
	return m, ok, nil
}

// NewStaticRunSource builds a source from an id→RunMeta map.
func NewStaticRunSource(runs map[string]RunMeta) *StaticRunSource {
	cp := make(map[string]RunMeta, len(runs))
	for k, v := range runs {
		cp[k] = v
	}
	return &StaticRunSource{runs: cp}
}

// staticRunRow is the on-disk JSON shape (TeamID as a string so the file is hand-editable).
type staticRunRow struct {
	RunID     string `json:"runId"`
	TeamID    string `json:"teamId"`
	Principal string `json:"principal"`
	RepoPath  string `json:"repoPath"`
	HeadRef   string `json:"headRef"`
	BaseRef   string `json:"baseRef"`
}

// LoadStaticRuns reads a JSON array of runs into a StaticRunSource. Every field is required; a bad
// row fails closed (the whole file is rejected) so a dev run never silently loses tenancy scope.
func LoadStaticRuns(path string) (*StaticRunSource, error) {
	// #nosec G304 -- operator-supplied dev config path (KSQUAD_DEV_RUNS), not request-tainted input.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dev runs %q: %w", path, err)
	}
	var rows []staticRunRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse dev runs %q: %w", path, err)
	}
	runs := make(map[string]RunMeta, len(rows))
	for i, row := range rows {
		if row.RunID == "" || row.TeamID == "" || row.Principal == "" || row.RepoPath == "" || row.HeadRef == "" || row.BaseRef == "" {
			return nil, fmt.Errorf("dev run %d: runId, teamId, principal, repoPath, headRef and baseRef are all required", i)
		}
		team, perr := uuid.Parse(row.TeamID)
		if perr != nil {
			return nil, fmt.Errorf("dev run %d: invalid teamId %q: %w", i, row.TeamID, perr)
		}
		runs[row.RunID] = RunMeta{
			RunID:     row.RunID,
			TeamID:    team,
			Principal: row.Principal,
			RepoPath:  row.RepoPath,
			HeadRef:   row.HeadRef,
			BaseRef:   row.BaseRef,
		}
	}
	return NewStaticRunSource(runs), nil
}
