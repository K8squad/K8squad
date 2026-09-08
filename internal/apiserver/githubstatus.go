package apiserver

// GitHub-status read model (ISI-3956 S5b, ADR-0013 §D1). The GitHub-status tab
// reads the scm.mirror_record MIRROR, never GitHub directly: every entity
// (PRs/issues/check-runs/artifacts/releases) is already fetched, normalized,
// rate-limit-handled, webhook+poll-refreshed and persisted by the operator's
// reposync reconciler behind the pkg/scm seam. So this is a read model over the
// mirror — the same one-writer/many-readers posture the dashboard already trusts
// (§6). There is NO second GitHub client, poller, or TTL cache here (Options
// B/C/D rejected): the mirror IS the cache, and its freshness IS the tab's.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/scm"
	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ============================================================================
// Wire model — the projection the tab (S5c) consumes
// ============================================================================

// GithubStatus is the single composed payload behind the GitHub-status tab. It
// projects the four/five mirror record kinds plus honest mirror freshness. The
// BYO repo credential is NEVER present (the mirror never stores it, §Tenancy).
type GithubStatus struct {
	Project      ProjectRef       `json:"project"`
	PullRequests []GithubPR       `json:"pullRequests"`
	Issues       []GithubIssue    `json:"issues"`
	CheckRuns    []GithubCheck    `json:"checkRuns"`
	Artifacts    []GithubArtifact `json:"artifacts"`
	Releases     []GithubRelease  `json:"releases"`
	Freshness    GithubFreshness  `json:"freshness"`
}

// GithubPR is one mirrored PR. ReviewState maps the raw provider state onto a
// mini-board column (ready-for-review | merged) best-effort from what the
// mirror carries; State is the raw open/closed for the tab to render verbatim.
type GithubPR struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	ReviewState string     `json:"reviewState,omitempty"`
	Merged      bool       `json:"merged,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	URL         string     `json:"url,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// GithubIssue is one mirrored issue.
type GithubIssue struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	URL       string     `json:"url,omitempty"`
	Actor     string     `json:"actor,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// GithubCheck is one mirrored check-run. Conclusion is the CI verdict
// (success | failure | ...) once State is "completed".
type GithubCheck struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
}

// GithubArtifact is one mirrored build artifact.
type GithubArtifact struct {
	Name      string     `json:"name"`
	URL       string     `json:"url,omitempty"`
	SizeBytes int64      `json:"sizeBytes,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// GithubRelease is one mirrored release (present iff S5a populated releases).
// State is draft | prerelease | published; Tag is the underlying git tag.
type GithubRelease struct {
	Name        string     `json:"name"`
	Tag         string     `json:"tag,omitempty"`
	State       string     `json:"state"`
	URL         string     `json:"url,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
}

// GithubFreshness is the honest mirror freshness the tab renders as "synced Ns
// ago" (ADR-0013 §D4) — never a fabricated "live" badge. Nil timestamps mean
// the mirror has not yet synced.
type GithubFreshness struct {
	LastMirrorTime    *time.Time `json:"lastMirrorTime,omitempty"`
	LastWebhookTime   *time.Time `json:"lastWebhookTime,omitempty"`
	MirrorRecordCount int64      `json:"mirrorRecordCount"`
}

// ============================================================================
// Service — projects the mirror; NO GitHub call, NO credential
// ============================================================================

// GithubStatusService is the S5b read model. It resolves the Project with the
// exact dashboard tenancy posture (admin fleet-wide short-circuit; else
// team-fenced existence-hiding 404) and projects that Project's scm mirror rows.
// Its ONLY data dependencies are the informer cache (resolution + freshness) and
// the mirror reader — never a pkg/scm provider client, so the request path makes
// zero outbound GitHub calls (AC2/AC6).
type GithubStatusService struct {
	reader client.Reader
	mirror scm.MirrorReader
}

// NewGithubStatusService builds the read model. Both dependencies are required;
// when the mirror reader is unavailable the caller leaves opts.GithubStatus nil
// and the route answers the documented 501 (AC5) rather than constructing a
// half-wired service.
func NewGithubStatusService(reader client.Reader, mirror scm.MirrorReader) *GithubStatusService {
	return &GithubStatusService{reader: reader, mirror: mirror}
}

// GithubStatus composes the payload for one Project. Resolution and every read
// are scoped strictly to the resolved (namespace, name); a foreign/unknown
// Project is ErrProjectNotFound → 404 (existence-hiding), a global admin is
// supra-tenant, and a bare name spanning squads is ErrProjectAmbiguous → 409.
func (s *GithubStatusService) GithubStatus(ctx context.Context, auth discussion.AuthorContext, projectID string) (GithubStatus, error) {
	ns, name, err := resolveProjectForAuth(ctx, s.reader, auth, projectID)
	if err != nil {
		return GithubStatus{}, err
	}

	out := GithubStatus{
		Project: ProjectRef{Name: name, Namespace: ns},
		// Non-nil empty slices so the payload marshals [] not null — the tab
		// types every panel as a non-null array.
		PullRequests: []GithubPR{},
		Issues:       []GithubIssue{},
		CheckRuns:    []GithubCheck{},
		Artifacts:    []GithubArtifact{},
		Releases:     []GithubRelease{},
	}

	rows, err := s.mirror.ListRecords(ctx, ns, name)
	if err != nil {
		return GithubStatus{}, err
	}
	for i := range rows {
		projectRow(&out, &rows[i])
	}

	// Freshness from the resolved Project's status.sync — the console's
	// freshness IS the mirror's freshness (AC3). A Project with no sync slice
	// yet leaves the timestamps nil (the tab renders "not synced yet").
	var proj ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &proj); err == nil {
		if sync := proj.Status.Sync; sync != nil {
			if sync.LastMirrorTime != nil {
				t := sync.LastMirrorTime.Time
				out.Freshness.LastMirrorTime = &t
			}
			if sync.LastWebhookTime != nil {
				t := sync.LastWebhookTime.Time
				out.Freshness.LastWebhookTime = &t
			}
			out.Freshness.MirrorRecordCount = sync.MirrorRecordCount
		}
	}

	return out, nil
}

// projectRow appends one mirror row onto the matching panel. Unknown kinds are
// dropped rather than mis-projected.
func projectRow(out *GithubStatus, row *scm.MirrorRow) {
	p := decodePayload(row.Payload)
	switch row.Kind {
	case scm.RecordTypePR:
		out.PullRequests = append(out.PullRequests, GithubPR{
			Number:      p.Number,
			Title:       row.Title,
			State:       row.State,
			ReviewState: prReviewState(row.State, p.Merged),
			Merged:      p.Merged,
			Branch:      p.HeadRef,
			URL:         p.URL,
			Actor:       row.Actor,
			UpdatedAt:   nonZeroTime(p.UpdatedAt),
		})
	case scm.RecordTypeIssue:
		out.Issues = append(out.Issues, GithubIssue{
			Number:    p.Number,
			Title:     row.Title,
			State:     row.State,
			URL:       p.URL,
			Actor:     row.Actor,
			UpdatedAt: nonZeroTime(p.UpdatedAt),
		})
	case scm.RecordTypeCheckRun:
		out.CheckRuns = append(out.CheckRuns, GithubCheck{
			Name:       row.Title,
			State:      row.State,
			Conclusion: p.Conclusion,
			URL:        p.URL,
		})
	case scm.RecordTypeArtifact:
		out.Artifacts = append(out.Artifacts, GithubArtifact{
			Name:      row.Title,
			URL:       p.URL,
			SizeBytes: p.Size,
			CreatedAt: nonZeroTime(p.CreatedAt),
			ExpiresAt: nonZeroTime(p.ExpiresAt),
		})
	case scm.RecordTypeRelease:
		out.Releases = append(out.Releases, GithubRelease{
			Name:        row.Title,
			Tag:         p.HeadRef,
			State:       row.State,
			URL:         p.URL,
			Actor:       row.Actor,
			PublishedAt: nonZeroTime(p.CreatedAt),
		})
	}
}

// prReviewState maps the raw provider PR state + merged flag onto a mini-board
// column, best-effort from what the mirror carries (§5.4). A merged PR is
// "merged"; an open PR is "ready-for-review"; anything else (e.g. closed-
// unmerged) is left blank so the board never mis-buckets it.
func prReviewState(state string, merged bool) string {
	switch {
	case merged:
		return PRMerged
	case state == "open":
		return PRReadyForReview
	default:
		return ""
	}
}

// decodePayload best-effort decodes a mirror row's payload; a nil/garbage
// payload yields a zero MirrorPayload rather than an error (the row's own
// columns still project).
func decodePayload(raw json.RawMessage) scm.MirrorPayload {
	var p scm.MirrorPayload
	if len(raw) == 0 {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

// nonZeroTime returns a pointer to t, or nil when t is the zero time — so a
// missing timestamp marshals as an absent field, not the Go epoch.
func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ============================================================================
// Handler — GET /api/projects/{projectId}/github behind the §13 choke point
// ============================================================================

// projectGithubStatus is the handler behind the route. BFFAuthz has already
// resolved the AuthorContext; the projection reads NOTHING from the request
// except the path variable. Statuses mirror the dashboard: 401 unauthenticated,
// 404 no-team-scope / foreign-or-unknown Project (existence-hiding), 409 admin
// name-ambiguity, 200 with the projected mirror.
func (s *Server) projectGithubStatus(svc *GithubStatusService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := mux.Vars(r)["projectId"]
		status, err := svc.GithubStatus(r.Context(), auth, projectID)
		switch {
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no github status for this team scope")
		case errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "no github status for this project")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous across squads; address by uid")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "github status read model unavailable")
		default:
			writeJSON(w, http.StatusOK, status)
		}
	}
}
