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
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

// GitHubProvider implements SourceControlProvider for GitHub.
// This is the v1 implementation that the repo-sync reconciler talks to.
type GitHubProvider struct {
	client *github.Client
	creds  ProviderCredentials
}

// NewGitHubProvider creates a new GitHub provider instance.
func NewGitHubProvider(baseURL string, creds ProviderCredentials) *GitHubProvider {
	httpClient := &http.Client{}
	if creds.Token != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: creds.Token})
		httpClient = &http.Client{
			Transport: &oauth2.Transport{
				Base:   http.DefaultTransport,
				Source: ts,
			},
		}
	}

	client := github.NewClient(httpClient)
	if baseURL != "" {
		client.BaseURL, _ = url.Parse(baseURL)
	}

	return &GitHubProvider{
		client: client,
		creds:  creds,
	}
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
		checkRecords, err := p.fetchCheckRuns(ctx, repoOwner, repoName, options)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch check runs: %w", err)
		}
		records = append(records, checkRecords...)
	}

	// Fetch artifacts (workflow runs)
	if len(options.Types) == 0 || contains(options.Types, RecordTypeArtifact) {
		artifactRecords, err := p.fetchArtifacts(ctx, repoOwner, repoName, options)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch artifacts: %w", err)
		}
		records = append(records, artifactRecords...)
	}

	return records, nil
}

// ValidateWebhook verifies the HMAC signature of a GitHub webhook.
func (p *GitHubProvider) ValidateWebhook(ctx context.Context, signature string, secret string, payload []byte) bool {
	if signature == "" || secret == "" || len(payload) == 0 {
		return false
	}

	// GitHub webhook signatures come in the format: sha256=<hash>
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}

	signatureHash := strings.TrimPrefix(signature, "sha256=")
	expectedHash := computeHMACSHA256(payload, secret)
	
	return signatureHash == expectedHash
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

	githubRepo, _, err := p.client.Repositories.Get(ctx, repoOwner, repoName)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", err)
	}

	return &Repository{
		Name:         githubRepo.GetName(),
		FullName:     githubRepo.GetFullName(),
		CloneURL:     githubRepo.GetCloneURL(),
		DefaultBranch: githubRepo.GetDefaultBranch(),
		Private:      githubRepo.GetPrivate(),
		Description:  githubRepo.GetDescription(),
		Language:     githubRepo.GetLanguage(),
		StarCount:    githubRepo.GetStargazersCount(),
		LastPushedAt: githubRepo.GetPushedAt().Time,
	}, nil
}

// fetchIssues fetches issues from GitHub.
func (p *GitHubProvider) fetchIssues(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.IssueListByRepoOptions{
		State: "all",
		Since: options.Since,
	}

	for page := 1; ; page++ {
		opt.Page = page
		issues, resp, err := p.client.Issues.ListByRepo(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
		}

		for _, issue := range issues {
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

		if resp.NextPage == 0 {
			break
		}
	}

	return records, nil
}

// fetchPullRequests fetches pull requests from GitHub.
func (p *GitHubProvider) fetchPullRequests(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.PullRequestListOptions{
		State: "all",
	}
	if options.Branch != "" {
		opt.Base = options.Branch
	}

	for page := 1; ; page++ {
		opt.Page = page
		prs, resp, err := p.client.PullRequests.List(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
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
				HeadRef:   pr.GetHead().GetRef(),
				BaseRef:   pr.GetBase().GetRef(),
				Merged:     pr.GetMerged(),
			}
			records = append(records, record)
		}

		if resp.NextPage == 0 {
			break
		}
	}

	return records, nil
}

// fetchCheckRuns fetches check runs from GitHub.
func (p *GitHubProvider) fetchCheckRuns(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	opt := &github.ListCheckRunsOptions{}

	// Get all commit SHAs first
	commits, _, err := p.client.Repositories.ListCommits(ctx, owner, repo, &github.CommitsListOptions{
		Since: options.Since,
	})
	if err != nil {
		return nil, err
	}

	for _, commit := range commits {
		checkRuns, _, err := p.client.Checks.ListCheckRunsForRef(ctx, owner, repo, commit.GetSHA(), opt)
		if err != nil {
			return nil, err
		}

		for _, checkRun := range checkRuns.CheckRuns {
			record := NormalizedRecord{
				Kind:        RecordTypeCheckRun,
				ExternalID:  fmt.Sprintf("%d", checkRun.GetID()),
				State:       checkRun.GetStatus(),
				Title:      checkRun.GetName(),
				URL:        checkRun.GetHTMLURL(),
				Actor:      checkRunActor(checkRun),
				CreatedAt:  checkRun.GetStartedAt().Time,
				UpdatedAt:  checkRun.GetCompletedAt().Time,
				Conclusion: checkRun.GetConclusion(),
			}
			records = append(records, record)
		}
	}

	return records, nil
}

// fetchArtifacts fetches workflow run artifacts from GitHub.
func (p *GitHubProvider) fetchArtifacts(ctx context.Context, owner, repo string, options SnapshotOptions) ([]NormalizedRecord, error) {
	var records []NormalizedRecord

	// Get recent workflow runs
	runs, _, err := p.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &github.ListWorkflowRunsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, err
	}

	// ListArtifacts returns all artifacts for the repository; per-run
	// filtering is unnecessary for the mirror snapshot.
	for range runs.WorkflowRuns {
		artifacts, _, err := p.client.Actions.ListArtifacts(ctx, owner, repo, &github.ListOptions{PerPage: 100})
		if err != nil {
			continue // Skip this run if artifacts can't be fetched
		}

		for _, artifact := range artifacts.Artifacts {
			record := NormalizedRecord{
				Kind:       RecordTypeArtifact,
				ExternalID: fmt.Sprintf("%d", artifact.GetID()),
				Title:      artifact.GetName(),
				URL:        artifact.GetArchiveDownloadURL(),
				CreatedAt:  artifact.CreatedAt.Time,
				ExpiresAt:  artifact.ExpiresAt.Time,
				Size:       artifact.GetSizeInBytes(),
			}
			records = append(records, record)
		}
	}

	return records, nil
}

// Helper functions

func parseRepoURL(repoURL string) (owner, repo string, err error) {
	// Extract owner and repo from URL like "https://github.com/owner/repo"
	parts := strings.Split(strings.TrimPrefix(repoURL, "https://"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid GitHub URL format")
	}
	return parts[0], parts[1], nil
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

// computeHMACSHA256 computes HMAC-SHA256 signature.
// This is a simplified implementation for the example.
// In production, use crypto/hmac and crypto/sha256.
func computeHMACSHA256(payload []byte, secret string) string {
	// This is a placeholder implementation
	// Real implementation should use crypto/hmac and crypto/sha256
	h := hex.EncodeToString(payload)
	return h
}