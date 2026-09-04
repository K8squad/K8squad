package prgate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-github/v58/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockGitHubClient is a mock implementation of the GitHub client interface
type MockGitHubClient struct {
	mock.Mock
}

func (m *MockGitHubClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, *github.Response, error) {
	args := m.Called(ctx, owner, repo, number)
	return args.Get(0).(*github.PullRequest), args.Get(1).(*github.Response), args.Error(2)
}

func (m *MockGitHubClient) ListPullRequestCommits(ctx context.Context, owner, repo string, number int, opts *github.ListOptions) ([]*github.RepositoryCommit, *github.Response, error) {
	args := m.Called(ctx, owner, repo, number, opts)
	return args.Get(0).([]*github.RepositoryCommit), args.Get(1).(*github.Response), args.Error(2)
}

func (m *MockGitHubClient) GetCheckRunsForRef(ctx context.Context, owner, repo string, ref string, opts *github.ListCheckRunsOptions) ([]*github.CheckRun, *github.Response, error) {
	args := m.Called(ctx, owner, repo, ref, opts)
	return args.Get(0).([]*github.CheckRun), args.Get(1).(*github.Response), args.Error(2)
}

func signedCommit() *github.RepositoryCommit {
	return createMockCommit("feat: test change\n\nSigned-off-by: testuser <test@example.com>")
}

func unsignedCommit() *github.RepositoryCommit {
	return createMockCommit("feat: test change without signature")
}

// Test cases for PR validation gate
func TestPRValidationGate(t *testing.T) {
	tests := []struct {
		name             string
		prData           *PullRequestData
		isNilPR          bool
		shouldPass       bool
		failedCheckNames []string
	}{
		{
			name: "Valid PR with all checks passing",
			prData: &PullRequestData{
				PRNumber:  189,
				Author:    "testuser",
				Title:     "test feature implementation",
				Body:      "Test PR body",
				BaseRef:   "main",
				HeadRef:   "feature/test",
				HasDCO:    true,
				CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "success"), createMockCheckRun("security", "completed", "success")},
				Coverage:  85.0,
				Commits:   []*github.RepositoryCommit{signedCommit()},
			},
			shouldPass: true,
		},
		{
			name: "PR fails DCO validation",
			prData: &PullRequestData{
				PRNumber:  190,
				Author:    "testuser",
				Title:     "test feature without DCO",
				Body:      "Test PR body",
				BaseRef:   "main",
				HeadRef:   "feature/test",
				HasDCO:    false,
				CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "success"), createMockCheckRun("security", "completed", "success")},
				Coverage:  85.0,
				Commits:   []*github.RepositoryCommit{unsignedCommit()},
			},
			shouldPass:       false,
			failedCheckNames: []string{"DCO Compliance"},
		},
		{
			name: "PR fails CI validation",
			prData: &PullRequestData{
				PRNumber:  191,
				Author:    "testuser",
				Title:     "test feature with CI failure",
				Body:      "Test PR body",
				BaseRef:   "main",
				HeadRef:   "feature/test",
				HasDCO:    true,
				CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "failure"), createMockCheckRun("security", "completed", "success")},
				Coverage:  85.0,
				Commits:   []*github.RepositoryCommit{signedCommit()},
			},
			shouldPass:       false,
			failedCheckNames: []string{"CI Status"},
		},
		{
			name: "PR fails coverage validation",
			prData: &PullRequestData{
				PRNumber:  192,
				Author:    "testuser",
				Title:     "test feature with low coverage",
				Body:      "Test PR body",
				BaseRef:   "main",
				HeadRef:   "feature/test",
				HasDCO:    true,
				CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "success"), createMockCheckRun("security", "completed", "success")},
				Coverage:  45.0, // Below threshold
				Commits:   []*github.RepositoryCommit{signedCommit()},
			},
			shouldPass:       false,
			failedCheckNames: []string{"Code Coverage"},
		},
		{
			name: "PR has missing check runs",
			prData: &PullRequestData{
				PRNumber:  193,
				Author:    "testuser",
				Title:     "test feature missing security check",
				Body:      "Test PR body",
				BaseRef:   "main",
				HeadRef:   "feature/test",
				HasDCO:    true,
				CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "success")}, // Missing security check
				Coverage:  85.0,
				Commits:   []*github.RepositoryCommit{signedCommit()},
			},
			shouldPass:       false,
			failedCheckNames: []string{"CI Status"},
		},
		{
			name:             "Nil PR data returns a SystemError",
			prData:           nil,
			isNilPR:          true,
			shouldPass:       false,
			failedCheckNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, err := NewPRValidationGate(&Config{
				MinCoverage:    55.0,
				RequiredChecks: []string{"ci/lint", "security"},
				Timeout:        30 * time.Second,
			})
			if err != nil {
				t.Fatalf("NewPRValidationGate: %v", err)
			}

			result, err := gate.Validate(context.Background(), tt.prData)

			if tt.isNilPR {
				assert.Error(t, err, "Expected a SystemError for nil PR data")
				assert.Nil(t, result)
				var validationErr *ValidationError
				assert.True(t, errors.As(err, &validationErr), "Expected ValidationError type")
				assert.Equal(t, SystemError, validationErr.Type)
				return
			}

			assert.NoError(t, err, "Expected validation to complete without error")
			assert.NotNil(t, result)
			assert.NotEmpty(t, result.CheckResults, "Expected check results")

			if tt.shouldPass {
				assert.True(t, result.IsValid, "Expected result to be valid")
			} else {
				assert.False(t, result.IsValid, "Expected result to be invalid")
				var failed []string
				for _, check := range result.CheckResults {
					if check.Status == "fail" {
						failed = append(failed, check.Name)
					}
				}
				assert.ElementsMatch(t, tt.failedCheckNames, failed, "Expected the failing checks to match")
			}
		})
	}
}

// Test edge cases
func TestPRValidationGateEdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		config       *Config
		expectError  bool
		errorMessage string
	}{
		{
			name: "Valid configuration",
			config: &Config{
				MinCoverage:    55.0,
				RequiredChecks: []string{"ci/lint", "security"},
				Timeout:        30 * time.Second,
			},
			expectError: false,
		},
		{
			name: "Invalid coverage threshold",
			config: &Config{
				MinCoverage:    101.0, // Invalid threshold
				RequiredChecks: []string{"ci/lint"},
				Timeout:        30 * time.Second,
			},
			expectError:  true,
			errorMessage: "invalid coverage threshold: must be between 0 and 100",
		},
		{
			name: "Negative coverage threshold",
			config: &Config{
				MinCoverage:    -10.0, // Invalid threshold
				RequiredChecks: []string{"ci/lint"},
				Timeout:        30 * time.Second,
			},
			expectError:  true,
			errorMessage: "invalid coverage threshold: must be between 0 and 100",
		},
		{
			name: "Empty required checks",
			config: &Config{
				MinCoverage:    55.0,
				RequiredChecks: []string{}, // Empty list
				Timeout:        30 * time.Second,
			},
			expectError:  true,
			errorMessage: "at least one required check must be specified",
		},
		{
			name: "Zero timeout",
			config: &Config{
				MinCoverage:    55.0,
				RequiredChecks: []string{"ci/lint"},
				Timeout:        0, // Zero timeout
			},
			expectError:  true,
			errorMessage: "timeout must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPRValidationGate(tt.config)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMessage)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Test gate with an injected GitHub client: validation must not depend on the
// GitHub API surface for locally complete PR data.
func TestPRValidationGateWithMockGitHub(t *testing.T) {
	gate, err := NewPRValidationGate(&Config{
		MinCoverage:    55.0,
		RequiredChecks: []string{"ci/lint", "security"},
		Timeout:        10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPRValidationGate: %v", err)
	}
	gate.SetGitHubClient(&MockGitHubClient{})

	prData := &PullRequestData{
		PRNumber:  189,
		Author:    "testuser",
		Title:     "Test PR",
		Body:      "Test PR body",
		BaseRef:   "main",
		HeadRef:   "feature/test",
		HasDCO:    true,
		CheckRuns: []*github.CheckRun{createMockCheckRun("ci/lint", "completed", "success"), createMockCheckRun("security", "completed", "success")},
		Coverage:  85.0,
		Commits:   []*github.RepositoryCommit{signedCommit()},
	}

	result, err := gate.Validate(context.Background(), prData)

	assert.NoError(t, err)
	assert.True(t, result.IsValid)
	assert.Equal(t, 4, len(result.CheckResults))
}

// Benchmark tests
func BenchmarkPRValidationGate(b *testing.B) {
	gate, err := NewPRValidationGate(&Config{
		MinCoverage:    55.0,
		RequiredChecks: []string{"ci/lint", "security", "integration"},
		Timeout:        30 * time.Second,
	})
	if err != nil {
		b.Fatalf("NewPRValidationGate: %v", err)
	}

	prData := &PullRequestData{
		PRNumber: 189,
		Author:   "testuser",
		Title:    "test feature implementation",
		Body:     "Test PR body",
		BaseRef:  "main",
		HeadRef:  "feature/test",
		HasDCO:   true,
		CheckRuns: []*github.CheckRun{
			createMockCheckRun("ci/lint", "completed", "success"),
			createMockCheckRun("security", "completed", "success"),
			createMockCheckRun("integration", "completed", "success"),
		},
		Coverage: 85.0,
		Commits:  []*github.RepositoryCommit{signedCommit()},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gate.Validate(context.Background(), prData)
	}
}

// Helper functions
func createMockCheckRun(name, status, conclusion string) *github.CheckRun {
	return &github.CheckRun{
		ID:         github.Int64(1),
		Name:       github.String(name),
		Status:     github.String(status),
		Conclusion: github.String(conclusion),
	}
}

func createMockCommit(message string) *github.RepositoryCommit {
	return &github.RepositoryCommit{
		Commit: &github.Commit{
			Message: github.String(message),
		},
	}
}
