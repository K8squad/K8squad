/*
Copyright 2026 KSquad.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

// githubHTTPTimeout bounds every provider API call. A snapshot fans out to
// several list endpoints; without a client-level timeout a wedged upstream
// stalls the reconcile worker until the context deadline.
const githubHTTPTimeout = 30 * time.Second

// githubPerPage is the page size for every list call — GitHub's default is
// 30, which triples the round trips on any real repository.
const githubPerPage = 100

// maxRecordsPerKind bounds one fetcher's in-memory accumulation: the snapshot
// path holds whole result sets before writing, so a cap keeps the operator's
// footprint bounded on very large repositories. Pagination stops once the cap
// is reached; the next poll tick picks up whatever changed.
const maxRecordsPerKind = 10000

// maxCheckRunRefs bounds the per-ref fan-out in fetchCheckRuns (default
// branch + open PR heads). Each ref costs one paginated list call.
const maxCheckRunRefs = 100

// RateLimitedError is returned when GitHub signals a primary rate limit or
// an abuse/secondary rate limit. The reconciler honours RetryAfter instead
// of fighting controller-runtime backoff against it.
type RateLimitedError struct {
	RetryAfter time.Duration
	cause      error
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("github rate limited, retry after %s: %v", e.RetryAfter, e.cause)
}

func (e *RateLimitedError) Unwrap() error { return e.cause }

// wrapRateLimit converts go-github rate-limit errors into RateLimitedError
// so callers can schedule a respectful retry; other errors pass through.
func wrapRateLimit(err error) error {
	if err == nil {
		return nil
	}
	var primary *github.RateLimitError
	if errors.As(err, &primary) {
		retryAfter := time.Until(primary.Rate.Reset.Time)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return &RateLimitedError{RetryAfter: retryAfter, cause: err}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		retryAfter := time.Second
		if abuse.RetryAfter != nil && *abuse.RetryAfter > retryAfter {
			retryAfter = *abuse.RetryAfter
		}
		return &RateLimitedError{RetryAfter: retryAfter, cause: err}
	}
	return err
}

// GitHubProvider implements SourceProvider for GitHub.
// This is the v1 implementation that the repo-sync reconciler talks to.
type GitHubProvider struct {
	client *github.Client
	creds  ProviderCredentials
}

// NewGitHubProvider creates a new GitHub provider instance. A malformed
// baseURL is returned as an error — never silently swallowed (the old
// `client.BaseURL, _ = url.Parse(baseURL)` dropped it and pointed every
// call at github.com).
func NewGitHubProvider(baseURL string, creds ProviderCredentials) (*GitHubProvider, error) {
	transport := http.DefaultTransport
	if creds.Token != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: creds.Token})
		transport = &oauth2.Transport{
			Base:   http.DefaultTransport,
			Source: ts,
		}
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   githubHTTPTimeout,
	}

	client := github.NewClient(httpClient)
	if baseURL != "" {
		// WithEnterpriseURLs handles the /api/v3/ suffix and upload URL
		// normalization; a hand-rolled BaseURL assignment silently skips both.
		var err error
		client, err = client.WithEnterpriseURLs(baseURL, baseURL)
		if err != nil {
			return nil, fmt.Errorf("invalid provider baseURL %q: %w", baseURL, err)
		}
	}

	return &GitHubProvider{
		client: client,
		creds:  creds,
	}, nil
}

// Name returns "github".
func (p *GitHubProvider) Name() string {
	return "github"
}

// Snapshot fetches the current state of GitHub repository objects.
func (p *GitHubProvider) Snapshot(ctx context.Context, repoURL string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	repoOwner, repoName, err := parseRepoURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf("invalid repo URL: %w", err)
	}

	// Fetch issues
	if len(options.Types) == 0 || contains(options.Types, RecordTypeIssue) {
		issueRecords, err := p.fetchIssues(ctx, repoOwner, repoName, options)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch issues: %w", err)
		}
		records = append(records, issueRecords...)
	}

	// Fetch PRs
	if len(options.Types) == 0 || contains(options.Types, RecordTypePR) {
		prRecords, err := p.fetchPullRequests(ctx, repoOwner, repoName, options)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch pull requests: %w", err)
		}
		records = append(records, prRecords...)
	}

	// Fetch check runs
	if len(options.Types) == 0 || contains(options.Types, RecordTypeCheckRun) {
		checkRecords, err := p.fetchCheckRuns(ctx, repoOwner, repoName)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch check runs: %w", err)
		}
		records = append(records, checkRecords...)
	}

	// Fetch artifacts
	if len(options.Types) == 0 || contains(options.Types, RecordTypeArtifact) {
		artifactRecords, err := p.fetchArtifacts(ctx, repoOwner, repoName)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch artifacts: %w", err)
		}
		records = append(records, artifactRecords...)
	}

	return records, nil
}

// GitHub webhook delivery headers — provider knowledge that lives here,
// behind the seam, never in the ingress (story 11.5).
const (
	githubSignatureHeader = "X-Hub-Signature-256"
	githubEventHeader     = "X-GitHub-Event"
	githubDeliveryHeader  = "X-GitHub-Delivery"
)

// VerifyWebhookDelivery authenticates a GitHub webhook delivery: the
// X-Hub-Signature-256 header ("sha256=<hex>") is parsed and compared
// against the HMAC-SHA256 of the payload under the per-Project secret.
// It delegates to the canonical implementation in hmac.go (story 11.1
// AC4, D8/NFR-SEC8): a second, divergent copy of the verify logic is
// exactly how a verify/parse skew bug is born. Absent or malformed
// header, empty secret, or empty payload all verify false — the caller
// drops the delivery, never falls back to an unsigned parse.
func (p *GitHubProvider) VerifyWebhookDelivery(_ context.Context, headers http.Header, payload []byte, secret string) bool {
	if headers == nil {
		return false
	}
	digest, err := ParseSignatureHeader(headers.Get(githubSignatureHeader))
	if err != nil {
		return false
	}
	return VerifyHMAC(payload, secret, digest)
}

// GitHubWebhookExtractor implements WebhookExtractor for GitHub deliveries.
// It is stateless — one value serves every Project — and reads only GitHub's
// own delivery headers (X-Hub-Signature-256, X-GitHub-Event). A GitLab
// extractor would read X-Gitlab-Token / X-Gitlab-Event instead, with no
// change to the webhook handler (Story 11.5).
type GitHubWebhookExtractor struct{}

// Signature extracts the bare HMAC digest from GitHub's X-Hub-Signature-256
// header. It reads headers only — the body is unverified at this point in the
// AC4 pipeline and must not be touched.
func (GitHubWebhookExtractor) Signature(header http.Header) (string, error) {
	return ParseSignatureHeader(header.Get("X-Hub-Signature-256"))
}

// Event returns GitHub's X-GitHub-Event header, falling back to a probe of
// the (already-verified) body when the header is absent. Logging/trigger use
// only — the reconcile is level-triggered and never trusts the payload.
func (GitHubWebhookExtractor) Event(header http.Header, body []byte) string {
	if e := header.Get("X-GitHub-Event"); e != "" {
		return e
	}
	return githubEventFromPayload(body)
}

// githubEventFromPayload inspects an already-verified delivery body for an
// event hint when the X-GitHub-Event header is absent (logging only).
// Unparseable payloads still trigger a reconcile upstream — the reconcile is
// level-triggered and never trusts the payload (Story 11.1 AC2).
func githubEventFromPayload(body []byte) string {
	var probe struct {
		Zen     string `json:"zen"`
		Action  string `json:"action"`
		PullReq *struct {
			Title string `json:"title"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "unknown"
	}
	switch {
	case probe.Zen != "":
		return "ping"
	case probe.PullReq != nil:
		return "pull_request/" + probe.Action
	default:
		return "unknown"
	}
}

// ParseWebhookEvent summarizes a VERIFIED GitHub delivery. The event type
// comes from X-GitHub-Event; when the header is absent (e.g. a proxied or
// hand-rolled delivery) the already-verified payload is probed for a hint —
// logging-grade attribution only, since the triggered reconcile is
// level-triggered and never trusts the payload (story 11.1 AC2).
func (p *GitHubProvider) ParseWebhookEvent(_ context.Context, headers http.Header, payload []byte) (*WebhookEvent, error) {
	event := &WebhookEvent{
		Type:       "unknown",
		DeliveryID: headers.Get(githubDeliveryHeader),
	}
	eventType := ""
	if headers != nil {
		eventType = headers.Get(githubEventHeader)
	}
	if eventType == "" {
		eventType = probeGitHubPayload(payload)
	}
	event.Type = eventType
	return event, nil
}

// probeGitHubPayload guesses the event type from a verified payload body.
// It recognizes only the shapes the ingress historically logged (ping zen,
// pull_request objects); anything else stays "unknown" — an unattributed
// delivery still triggers the reconcile.
func probeGitHubPayload(payload []byte) string {
	var probe struct {
		Zen     string `json:"zen"`
		Action  string `json:"action"`
		PullReq *struct {
			Title string `json:"title"`
		} `json:"pull_request"`
		Issue *struct {
			Title string `json:"title"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "unknown"
	}
	switch {
	case probe.Zen != "":
		return "ping"
	case probe.PullReq != nil:
		return "pull_request/" + probe.Action
	case probe.Issue != nil:
		return "issue/" + probe.Action
	default:
		return "unknown"
	}
}

// CreateComment creates a comment on a GitHub issue or PR.
func (p *GitHubProvider) CreateComment(ctx context.Context, repoURL string, kind string, externalID string, comment string) (string, error) {
	repoOwner, repoName, err := parseRepoURL(repoURL)
	if err != nil {
		return "", fmt.Errorf("invalid repo URL: %w", err)
	}

	var issueNum int
	switch kind {
	case "issue":
		issueNum, err = parseExternalID(externalID)
		if err != nil {
			return "", fmt.Errorf("invalid issue ID: %w", err)
		}
	case "pr":
		issueNum, err = parseExternalID(externalID)
		if err != nil {
			return "", fmt.Errorf("invalid PR ID: %w", err)
		}
	default:
		return "", fmt.Errorf("unsupported comment kind: %s", kind)
	}

	githubComment, _, err := p.client.Issues.CreateComment(ctx, repoOwner, repoName, issueNum, &github.IssueComment{
		Body: &comment,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create comment: %w", err)
	}

	return fmt.Sprintf("%d", githubComment.GetID()), nil
}

// CreateStatus creates a status on a GitHub commit or PR.
func (p *GitHubProvider) CreateStatus(ctx context.Context, repoURL string, sha string, status Status) error {
	repoOwner, repoName, err := parseRepoURL(repoURL)
	if err != nil {
		return fmt.Errorf("invalid repo URL: %w", err)
	}

	githubStatus := &github.RepoStatus{
		State:       &status.State,
		Context:     &status.Context,
		Description: &status.Description,
		TargetURL:   &status.TargetURL,
	}

	_, _, err = p.client.Repositories.CreateStatus(ctx, repoOwner, repoName, sha, githubStatus)
	return err
}

// GetRepo fetches GitHub repository information.
func (p *GitHubProvider) GetRepo(ctx context.Context, repoURL string) (*Repository, error) {
	repoOwner, repoName, err := parseRepoURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf("invalid repo URL: %w", err)
	}

	githubRepo, resp, err := p.client.Repositories.Get(ctx, repoOwner, repoName)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", wrapRateLimit(err))
	}
	_ = resp

	return &Repository{
		Name:          githubRepo.GetName(),
		FullName:      githubRepo.GetFullName(),
		CloneURL:      githubRepo.GetCloneURL(),
		DefaultBranch: githubRepo.GetDefaultBranch(),
		Private:       githubRepo.GetPrivate(),
		Description:   githubRepo.GetDescription(),
		Language:      githubRepo.GetLanguage(),
		StarCount:     githubRepo.GetStargazersCount(),
		LastPushedAt:  githubRepo.GetPushedAt().Time,
	}, nil
}

// fetchIssues fetches issues from GitHub. GitHub's issues endpoint also
// returns pull requests as issues, so PRs are skipped here — they are
// mirrored once, as RecordTypePR, by fetchPullRequests.
func (p *GitHubProvider) fetchIssues(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.IssueListByRepoOptions{
		State: "all",
		Since: options.Since,
		ListOptions: github.ListOptions{
			PerPage: githubPerPage,
		},
	}

	for {
		issues, resp, err := p.client.Issues.ListByRepo(ctx, owner, repo, opt)
		if err != nil {
			return nil, wrapRateLimit(err)
		}

		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue // mirrored once, as a PR, by fetchPullRequests
			}
			record := NormalizedRecord{
				Kind:       RecordTypeIssue,
				ExternalID: fmt.Sprintf("%d", issue.GetNumber()),
				State:      issue.GetState(),
				Title:      issue.GetTitle(),
				Body:       issue.GetBody(),
				URL:        issue.GetHTMLURL(),
				Actor:      getGitHubActor(issue.GetUser()),
				CreatedAt:  issue.GetCreatedAt().Time,
				UpdatedAt:  issue.GetUpdatedAt().Time,
				Number:     issue.GetNumber(),
				Assignees:  getGitHubUsernames(issue.Assignees),
				Labels:     getGitHubLabels(issue.Labels),
			}
			records = append(records, record)
		}

		if resp.NextPage == 0 || len(records) >= maxRecordsPerKind {
			break
		}
		opt.Page = resp.NextPage
	}

	return records, nil
}

// fetchPullRequests fetches pull requests from GitHub.
func (p *GitHubProvider) fetchPullRequests(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.PullRequestListOptions{
		State: "all",
		ListOptions: github.ListOptions{
			PerPage: githubPerPage,
		},
	}
	if options.Branch != "" {
		opt.Base = options.Branch
	}

	for {
		prs, resp, err := p.client.PullRequests.List(ctx, owner, repo, opt)
		if err != nil {
			return nil, wrapRateLimit(err)
		}

		for _, pr := range prs {
			record := NormalizedRecord{
				Kind:       RecordTypePR,
				ExternalID: fmt.Sprintf("%d", pr.GetNumber()),
				State:      pr.GetState(),
				Title:      pr.GetTitle(),
				Body:       pr.GetBody(),
				URL:        pr.GetHTMLURL(),
				Actor:      getGitHubActor(pr.GetUser()),
				CreatedAt:  pr.GetCreatedAt().Time,
				UpdatedAt:  pr.GetUpdatedAt().Time,
				Number:     pr.GetNumber(),
				Assignees:  getGitHubUsernames(pr.Assignees),
				Labels:     getGitHubLabels(pr.Labels),
				HeadRef:    pr.GetHead().GetRef(),
				BaseRef:    pr.GetBase().GetRef(),
				Merged:     pr.GetMerged(),
			}
			records = append(records, record)
		}

		if resp.NextPage == 0 || len(records) >= maxRecordsPerKind {
			break
		}
		opt.Page = resp.NextPage
	}

	return records, nil
}

// fetchCheckRuns fetches check runs for the repository's current state: the
// default branch ref plus the head SHAs of open PRs (bounded by
// maxCheckRunRefs). Walking commit history per commit was both N+1 and
// incomplete (no pagination); scoping to live refs is cheaper and matches
// what the mirror actually consumes. ListCheckRunsForRef accepts a branch
// name directly, so no SHA resolution round trip is needed for the default
// branch. Duplicates (a check run visible on more than one ref) are
// deduplicated by check-run id.
func (p *GitHubProvider) fetchCheckRuns(ctx context.Context, owner, repo string) ([]NormalizedRecord, error) {
	repoInfo, _, err := p.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, wrapRateLimit(err)
	}

	refs := []string{repoInfo.GetDefaultBranch()}

	prOpt := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: githubPerPage},
	}
	for len(refs) < maxCheckRunRefs {
		prs, resp, err := p.client.PullRequests.List(ctx, owner, repo, prOpt)
		if err != nil {
			return nil, wrapRateLimit(err)
		}
		for _, pr := range prs {
			if sha := pr.GetHead().GetSHA(); sha != "" && len(refs) < maxCheckRunRefs {
				refs = append(refs, sha)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		prOpt.Page = resp.NextPage
	}

	var records []NormalizedRecord
	seen := map[int64]struct{}{}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		opt := &github.ListCheckRunsOptions{
			ListOptions: github.ListOptions{PerPage: githubPerPage},
		}
		for {
			checkRuns, resp, err := p.client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opt)
			if err != nil {
				return nil, wrapRateLimit(err)
			}
			for _, checkRun := range checkRuns.CheckRuns {
				id := checkRun.GetID()
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				record := NormalizedRecord{
					Kind:       RecordTypeCheckRun,
					ExternalID: fmt.Sprintf("%d", id),
					State:      checkRun.GetStatus(),
					Title:      checkRun.GetName(),
					URL:        checkRun.GetHTMLURL(),
					Actor:      checkRunActor(checkRun),
					CreatedAt:  checkRun.GetStartedAt().Time,
					UpdatedAt:  checkRun.GetCompletedAt().Time,
					Conclusion: checkRun.GetConclusion(),
				}
				records = append(records, record)
			}
			if resp.NextPage == 0 || len(records) >= maxRecordsPerKind {
				break
			}
			opt.Page = resp.NextPage
		}
	}

	return records, nil
}

// fetchArtifacts fetches repository artifacts. ListArtifacts is repo-wide
// (it takes no run identifier), so ONE paginated pass mirrors every
// artifact; the previous per-workflow-run loop burned a full API call per
// run for the same records.
func (p *GitHubProvider) fetchArtifacts(ctx context.Context, owner, repo string) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.ListOptions{PerPage: githubPerPage}
	for {
		artifacts, resp, err := p.client.Actions.ListArtifacts(ctx, owner, repo, opt)
		if err != nil {
			return nil, wrapRateLimit(err)
		}
		for _, artifact := range artifacts.Artifacts {
			record := NormalizedRecord{
				Kind:       RecordTypeArtifact,
				ExternalID: fmt.Sprintf("%d", artifact.GetID()),
				Title:      artifact.GetName(),
				URL:        artifact.GetArchiveDownloadURL(),
				CreatedAt:  time.Time{},
				ExpiresAt:  time.Time{},
				Size:       artifact.GetSizeInBytes(),
			}
			if artifact.CreatedAt != nil {
				record.CreatedAt = artifact.CreatedAt.Time
			}
			if artifact.ExpiresAt != nil {
				record.ExpiresAt = artifact.ExpiresAt.Time
			}
			records = append(records, record)
		}
		if resp.NextPage == 0 || len(records) >= maxRecordsPerKind {
			break
		}
		opt.Page = resp.NextPage
	}

	return records, nil
}

// Helper functions

// parseRepoURL extracts the owner and repository from a GitHub remote URL.
// It accepts https://host/owner/repo, host/owner/repo, and scp-style
// git@host:owner/repo forms (with or without a trailing .git), and always
// takes the LAST two path segments so a host with a port or a path-prefixed
// mirror still yields the right owner/repo.
func parseRepoURL(repoURL string) (owner, repo string, err error) {
	s := strings.TrimSuffix(strings.TrimSpace(repoURL), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		// Strip any userinfo (https://token@github.com/owner/repo).
		if slash := strings.Index(s, "/"); slash >= 0 {
			if at := strings.LastIndex(s[:slash], "@"); at >= 0 {
				s = s[at+1:]
			}
		}
	}
	s = strings.TrimPrefix(s, "git@")
	s = strings.Replace(s, ":", "/", 1) // scp-style git@host:owner/repo
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", repoURL)
	}
	owner, repo = parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" || strings.ContainsAny(owner, " ") || strings.ContainsAny(repo, " .") {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", repoURL)
	}
	return owner, repo, nil
}

func parseExternalID(id string) (int, error) {
	var num int
	_, err := fmt.Sscanf(id, "%d", &num)
	return num, err
}

func contains(types []RecordType, t RecordType) bool {
	for _, typ := range types {
		if typ == t {
			return true
		}
	}
	return false
}

func getGitHubActor(user *github.User) string {
	if user == nil {
		return ""
	}
	return user.GetLogin()
}

// checkRunActor maps a check run's App (GitHub Apps author check runs,
// not users) to the normalized actor field.
func checkRunActor(checkRun *github.CheckRun) string {
	if app := checkRun.GetApp(); app != nil {
		return app.GetSlug()
	}
	return ""
}

func getGitHubUsernames(users []*github.User) []string {
	var usernames []string
	for _, user := range users {
		if user != nil {
			usernames = append(usernames, user.GetLogin())
		}
	}
	return usernames
}

func getGitHubLabels(labels []*github.Label) []string {
	var names []string
	for _, label := range labels {
		if label != nil {
			names = append(names, label.GetName())
		}
	}
	return names
}
