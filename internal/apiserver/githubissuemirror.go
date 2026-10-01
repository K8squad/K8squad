package apiserver

import (
	"context"
	"encoding/json"

	"github.com/K8squad/K8squad/pkg/scm"
)

// sqlGithubIssueMirror is the production GithubIssueMirrorReader: a thin
// projection over the §5.4 scm mirror read seam (scm.MirrorReader) that the
// GitHub-status read model already uses (ISI-3956 S5b). It finds the one mirror
// row for an issue number under a resolved Project and decodes the body +
// comments out of the row's payload JSONB.
//
// It takes NO new dependency and issues NO GitHub call: every byte it returns
// was mirrored locally by the operator repo-sync relay (the only component that
// holds the PAT). This is exactly why the import lives here and not inline in the
// bridge — the apiserver cannot read the credential Secret, but it CAN read the
// mirror the operator wrote (ISI-5279 investigation, ISI-5308).
type sqlGithubIssueMirror struct {
	mirror scm.MirrorReader
}

// NewGithubIssueMirror binds the bridge's issue-import reader to the shared scm
// mirror reader (scm.NewSQLMirrorStore). A nil reader yields a nil result so the
// composition root can leave the bridge on its stub body in a mirror-less host.
func NewGithubIssueMirror(mirror scm.MirrorReader) GithubIssueMirrorReader {
	if mirror == nil {
		return nil
	}
	return &sqlGithubIssueMirror{mirror: mirror}
}

// MirroredIssue lists the Project's mirror rows and returns the one matching the
// issue number (the mirror external_id for issues is the bare number, per
// pkg/scm fetchIssues). ok=false when no such row exists yet.
func (s *sqlGithubIssueMirror) MirroredIssue(ctx context.Context, projectNamespace, projectName, number string) (MirroredIssue, bool, error) {
	rows, err := s.mirror.ListRecords(ctx, projectNamespace, projectName)
	if err != nil {
		return MirroredIssue{}, false, err
	}
	for i := range rows {
		row := &rows[i]
		if row.Kind != scm.RecordTypeIssue || row.ExternalID != number {
			continue
		}
		mi := MirroredIssue{Title: row.Title}
		if len(row.Payload) > 0 && string(row.Payload) != "null" {
			var p scm.MirrorPayload
			if jerr := json.Unmarshal(row.Payload, &p); jerr == nil {
				mi.Body = p.Body
				mi.URL = p.URL
				for _, c := range p.Comments {
					mi.Comments = append(mi.Comments, MirroredIssueComment{
						Actor:     c.Actor,
						Body:      c.Body,
						CreatedAt: c.CreatedAt,
					})
				}
			}
			// A malformed payload degrades to title-only rather than failing the
			// dispatch — the mirror body is best-effort enrichment.
		}
		return mi, true, nil
	}
	return MirroredIssue{}, false, nil
}
