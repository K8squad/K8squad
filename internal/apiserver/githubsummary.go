package apiserver

// Global GitHub summary read model (ISI-5705, WS-1b of the overview-UX redesign
// ISI-5697 / design ISI-5699). The Fleet overview's "GitHub strip" needs ONE
// fleet-wide roll-up of the squad's GitHub activity — issues assigned/open, PRs
// opened/reviewed/merged, red checks — plus a 7-day merge sparkline and a
// per-repo breakdown.
//
// CRITICAL sourcing rule (ISI-5472 / ISI-5612, memory github-rest-secondary-
// ratelimit): this is sourced ENTIRELY from the already-synced scm mirror — the
// SAME one-writer/many-readers mirror the per-project GitHub-status tab reads
// (githubstatus.go) — and NEVER a live per-project GitHub fan-out. A fleet-wide
// fan-out of REST calls would trip GitHub's secondary rate limit (which is
// IP-scoped and persists across heartbeats), starving CI's own status writes.
// The mirror IS the cache; its freshness IS this summary's. There is no GitHub
// client, poller, or credential on this path.
//
// It aggregates across exactly the Projects the caller may see — a global admin
// spans every squad (ADR-039 / ISI-3932), a tenant is fenced to their own Team —
// by delegating that scoping to the SAME SquadOverviewReader the Projects tab
// uses, so there is no bespoke tenancy logic to get wrong.
//
// Fabrication discipline (FR-I3): every count is 0-authoritative (the mirror
// holds no such row) rather than fabricated. prsReviewed is the ONE count that
// depends on the local review bridge (coord work items, a LOCAL DB read — NOT a
// GitHub call), so when that seam is unwired the reviewed counts are honestly 0
// and the FE degrades that panel per ISI-4229.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/reviewtrigger"
	"github.com/K8squad/K8squad/pkg/scm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ============================================================================
// Wire model — the projection the Fleet overview's GitHub strip consumes
// ============================================================================

// GithubSummary is the single fleet-wide GitHub roll-up behind GET
// /api/squad/github. Totals is the headline stat row; MergeSparkline7d drives
// the 7-day merge trend sparkline (oldest→newest, length 7); ByRepo is the
// per-repository breakdown (one row per distinct mirrored repo the caller can
// see), the surface that also supplies the real per-project "Open PRs" count
// that previously rendered a hard-coded "—".
type GithubSummary struct {
	Totals           GithubTotals     `json:"totals"`
	MergeSparkline7d []int            `json:"mergeSparkline7d"`
	ByRepo           []GithubRepoStat `json:"byRepo"`
}

// GithubTotals is the fleet headline row. Every field is a count derived purely
// from the mirror (prsReviewed additionally from the local review bridge, see
// the type doc). issuesAssigned counts OPEN issues that carry at least one
// GitHub assignee (the mirror's provider-normalized Assignees); issuesOpen is
// every open issue; prsOpened is every currently-open (non-merged) PR; prsMerged
// is every PR the mirror marks merged; prsReviewed is every PR with a local
// review work item that reached "done"; checksRed is every mirrored check-run
// whose conclusion is a failing verdict.
type GithubTotals struct {
	IssuesAssigned int `json:"issuesAssigned"`
	IssuesOpen     int `json:"issuesOpen"`
	PRsOpened      int `json:"prsOpened"`
	PRsReviewed    int `json:"prsReviewed"`
	PRsMerged      int `json:"prsMerged"`
	ChecksRed      int `json:"checksRed"`
}

// GithubRepoStat is one repository's slice of the fleet totals, keyed by the
// lower-cased owner/repo slug. Open is the repo's open-issue count; Assigned is
// its assigned open-issue count; PRs is its OPEN PR count (the authoritative
// per-project "Open PRs" the project overview renders instead of "—"); Reviewed
// is its reviewed-PR count; ChecksRed is its failing check-run count.
type GithubRepoStat struct {
	Repo      string `json:"repo"`
	Assigned  int    `json:"assigned"`
	Open      int    `json:"open"`
	PRs       int    `json:"prs"`
	Reviewed  int    `json:"reviewed"`
	ChecksRed int    `json:"checksRed"`
}

// mergeSparklineDays is the width of the merge trend sparkline (one bucket per
// day, including today) — a one-week glance the Fleet overview renders.
const mergeSparklineDays = 7

// ============================================================================
// Service — aggregates the mirror; NO GitHub call, NO credential
// ============================================================================

// GithubSummaryService is the WS-1b read model. Its data dependencies are the
// informer cache (project enumeration + repo URLs + per-project UID resolution,
// via the shared SquadOverviewReader) and the mirror reader — never a pkg/scm
// provider client, so the request path makes zero outbound GitHub calls. The
// optional reviews seam is a LOCAL coord DB read used only to count reviewed PRs.
type GithubSummaryService struct {
	reader   client.Reader
	mirror   scm.MirrorReader
	projects SquadOverviewReader
	// reviews is the OPTIONAL local review-bridge seam (reused from the S5b
	// per-project read model, ISI-4767 E5). Nil ⇒ prsReviewed / per-repo reviewed
	// are honestly 0 (no review bridge wired) rather than fabricated, and every
	// other count is unaffected. It is a LOCAL coord DB read, NOT a GitHub call.
	reviews ReviewItemFinder
	// now sources the wall clock used to bucket the merge sparkline; overridable
	// in tests. Defaulted to time.Now by the constructor.
	now func() time.Time
}

// NewGithubSummaryService builds the read model. reader + mirror + projects are
// required; when any is unavailable the caller leaves opts.GithubSummary nil and
// the route answers the documented 501 rather than constructing a half-wired
// service. reviews is optional (nil ⇒ reviewed counts are 0; every other count
// is unaffected).
func NewGithubSummaryService(reader client.Reader, mirror scm.MirrorReader, projects SquadOverviewReader, reviews ReviewItemFinder) *GithubSummaryService {
	return &GithubSummaryService{reader: reader, mirror: mirror, projects: projects, reviews: reviews, now: time.Now}
}

// repoAgg is the mutable per-repo accumulator keyed by owner/repo slug while a
// Summary pass folds every visible Project's mirror rows together.
type repoAgg struct {
	assigned  int
	open      int
	prs       int
	reviewed  int
	checksRed int
}

// Summary composes the fleet-wide GitHub roll-up for the caller. It enumerates
// the Projects the caller may see with the SAME tenancy posture as the Projects
// tab (admin ⇒ every squad; tenant ⇒ own Team, existence-hiding), then folds
// each Project's mirror rows into the totals + per-repo breakdown + merge
// sparkline. A single Project whose mirror read errors is skipped (degrade-don't-
// blank, ISI-4229) rather than failing the whole summary; only a failure to
// enumerate Projects at all propagates.
func (s *GithubSummaryService) Summary(ctx context.Context, auth discussion.AuthorContext) (GithubSummary, error) {
	list, err := s.projects.Projects(ctx, auth.TeamID.String(), auth.IsAdmin)
	if err != nil {
		return GithubSummary{}, err
	}

	out := GithubSummary{
		// Non-nil slices so the payload marshals [] not null — the FE types both
		// as non-null arrays.
		MergeSparkline7d: make([]int, mergeSparklineDays),
		ByRepo:           []GithubRepoStat{},
	}
	byRepo := map[string]*repoAgg{}
	reviewTeam := reviewTeamID(auth)

	// Day buckets for the merge sparkline: index 0 is the oldest day in the
	// window, index mergeSparklineDays-1 is today. Bucketed in UTC off the
	// start-of-today so the trend is stable regardless of request time of day.
	todayStart := startOfUTCDay(s.now())

	for i := range list.Projects {
		p := &list.Projects[i]
		rows, rErr := s.mirror.ListRecords(ctx, p.Namespace, p.Name)
		if rErr != nil {
			// Degrade-don't-blank: one project's mirror read failing must not blank
			// the whole fleet summary. Skip it; the others still aggregate.
			continue
		}

		slug := repoSlugFromURL(p.RepoURL)
		agg := byRepo[slug]
		if agg == nil {
			agg = &repoAgg{}
			if slug != "" {
				byRepo[slug] = agg
			}
		}

		for ri := range rows {
			row := &rows[ri]
			payload := decodePayload(row.Payload)
			switch row.Kind {
			case scm.RecordTypeIssue:
				if row.State == "open" {
					out.Totals.IssuesOpen++
					agg.open++
					if len(payload.Assignees) > 0 {
						out.Totals.IssuesAssigned++
						agg.assigned++
					}
				}
			case scm.RecordTypePR:
				switch {
				case payload.Merged:
					out.Totals.PRsMerged++
					s.bucketMerge(out.MergeSparkline7d, todayStart, payload.UpdatedAt)
				case row.State == "open":
					out.Totals.PRsOpened++
					agg.prs++
				}
			case scm.RecordTypeCheckRun:
				if isRedConclusion(payload.Conclusion) {
					out.Totals.ChecksRed++
					agg.checksRed++
				}
			}
		}

		// Reviewed PRs: one indexed coord read per mirrored PR (LOCAL DB, bounded
		// by the project's PR count — never a GitHub call). Done in a second pass
		// so the UID is resolved at most once per project and only when the seam is
		// wired and the repo slug is known (the anchor label is repo-derived).
		if s.reviews != nil && slug != "" && p.RepoURL != "" {
			uid := s.projectUID(ctx, p.Namespace, p.Name)
			if uid != "" {
				for ri := range rows {
					if rows[ri].Kind != scm.RecordTypePR {
						continue
					}
					num := decodePayload(rows[ri].Payload).Number
					if num <= 0 {
						continue
					}
					anchor := reviewtrigger.PRAnchorLabel(p.RepoURL, strconv.Itoa(num))
					if anchor == "" {
						continue
					}
					rec, err := s.reviews.FindWorkItemByLabel(ctx, reviewTeam, uid, anchor)
					if err != nil {
						continue // honest absence (not reviewed) or a transient read error
					}
					if rec.State == "done" {
						out.Totals.PRsReviewed++
						agg.reviewed++
					}
				}
			}
		}
	}

	// Flatten the per-repo map into a deterministic slice (sorted by slug) so the
	// FE renders a stable order and tests are not order-flaky.
	out.ByRepo = make([]GithubRepoStat, 0, len(byRepo))
	for slug, agg := range byRepo {
		out.ByRepo = append(out.ByRepo, GithubRepoStat{
			Repo:      slug,
			Assigned:  agg.assigned,
			Open:      agg.open,
			PRs:       agg.prs,
			Reviewed:  agg.reviewed,
			ChecksRed: agg.checksRed,
		})
	}
	sort.Slice(out.ByRepo, func(a, b int) bool { return out.ByRepo[a].Repo < out.ByRepo[b].Repo })

	// Enrich the inbound span with the fleet-level result shape so a GitHub-strip
	// read is queryable by scope + the headline counts; no per-entity ids.
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Bool("ksquad.scm.fleet", auth.IsAdmin),
		attribute.Int("ksquad.scm.result.repos", len(out.ByRepo)),
		attribute.Int("ksquad.scm.result.issues_open", out.Totals.IssuesOpen),
		attribute.Int("ksquad.scm.result.prs_open", out.Totals.PRsOpened),
		attribute.Int("ksquad.scm.result.prs_merged", out.Totals.PRsMerged),
		attribute.Int("ksquad.scm.result.checks_red", out.Totals.ChecksRed),
	)

	return out, nil
}

// projectUID resolves the Project CR object UID for (ns, name) from the informer
// cache — the key coord.work_item.project_id holds. Returns "" when the Project
// is not in the cache (the review join is then skipped for that project, the
// honest "not reviewed" default) rather than erroring the whole summary.
func (s *GithubSummaryService) projectUID(ctx context.Context, ns, name string) string {
	var proj ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &proj); err != nil {
		return ""
	}
	return string(proj.UID)
}

// bucketMerge folds one merged PR into the 7-day sparkline, keyed by the day of
// its last mirror update. The mirror does not carry a dedicated merged-at
// timestamp, so UpdatedAt is the honest best proxy for when the merge landed
// (a merge is the PR's terminal update). A PR whose update falls outside the
// window (older than 7 days, or a clock-skew future stamp) is simply not
// bucketed — never clamped into an edge bucket, which would fabricate a spike.
func (s *GithubSummaryService) bucketMerge(spark []int, todayStart time.Time, updatedAt time.Time) {
	if updatedAt.IsZero() {
		return
	}
	day := startOfUTCDay(updatedAt)
	daysAgo := int(todayStart.Sub(day).Hours() / 24)
	if daysAgo < 0 || daysAgo >= len(spark) {
		return
	}
	// index 0 = oldest day in the window, len-1 = today.
	spark[len(spark)-1-daysAgo]++
}

// startOfUTCDay truncates t to 00:00:00 UTC of its calendar day — the stable
// bucket key for the merge sparkline.
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// isRedConclusion reports whether a mirrored check-run conclusion is a failing
// ("red") verdict the fleet should surface. success / neutral / skipped are not
// red; a still-running check (empty conclusion) is not yet red either.
func isRedConclusion(conclusion string) bool {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "timed_out", "action_required", "startup_failure":
		return true
	default:
		return false
	}
}

// repoSlugFromURL normalizes a repo URL to its lower-cased owner/repo slug — the
// per-repo grouping key, matching the normalization reviewtrigger.PRAnchorLabel
// uses so the two agree. Returns "" for an absent/unparseable URL (the project
// is then omitted from the byRepo breakdown; its rows are empty in practice).
func repoSlugFromURL(repoURL string) string {
	s := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return ""
	}
	return strings.ToLower(owner + "/" + repo)
}

// ============================================================================
// Handler — GET /api/squad/github behind the §13 BFF authz choke point
// ============================================================================

// squadGithubSummary is the handler behind the route. BFFAuthz has already
// resolved the AuthorContext; the projection reads NOTHING from the request. A
// tenant whose Team scope resolves to nothing is 404 (existence-hiding, matching
// the Projects tab); any other read-model failure is 502.
func (s *Server) squadGithubSummary(svc *GithubSummaryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		summary, err := svc.Summary(r.Context(), auth)
		switch {
		case errors.Is(err, ErrTeamNotFound):
			writeJSONError(w, http.StatusNotFound, "no github summary for this team scope")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "github summary read model unavailable")
		default:
			writeJSON(w, http.StatusOK, summary)
		}
	}
}
