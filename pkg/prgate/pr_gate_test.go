package prgate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-github/v58/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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

func (m *MockGitHubClient) GetCheckRunsForRef(ctx context.Context, owner, repo, ref string, opts *github.ListCheckRunsOptions) ([]*github.CheckRun, *github.Response, error) {
	args := m.Called(ctx, owner, repo, ref, opts)
	return args.Get(0).([]*github.CheckRun), args.Get(1).(*github.Response), args.Error(2)
}

// Test cases for PR validation gate
func TestPRValidationGate(t *testing.T) {
	tests := []struct {
		name          string
		prData        *PullRequestData
		expectedError  error
		shouldPass    bool
		errorType     ValidationErrorType
	}{
		{
			name: "Valid PR with all checks passing",
			prData: &PullRequestData{
				PRNumber:      189,
				Author:        "testuser",
				Title:         "test feature implementation",
				Body:          "Test PR body",
				BaseRef:       "main",
				HeadRef:       "feature/test",
				HasDCO:        true,
				CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "success", "")},
				Coverage:      85.0,
				Commits:       []*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")},
			},
			shouldPass: true,
		},
		{
			name: "PR fails DCO validation",
			prData: &PullRequestData{
				PRNumber:      190,
				Author:        "testuser",
				Title:         "test feature without DCO",
				Body:          "Test PR body",
				BaseRef:       "main",
				HeadRef:       "feature/test",
				HasDCO:        false,
				CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "success", "")},
				Coverage:      85.0,
				Commits:       []*github.RepositoryCommit{createMockCommit("No DCO signature")},
			},
			expectedError:  &ValidationError{Type: DCOValidation, Message: "PR commits are not properly signed off"},
			shouldPass:    false,
			errorType:     DCOValidation,
		},
		{
			name: "PR fails CI validation",
			prData: &PullRequestData{
				PRNumber:      191,
				Author:        "testuser",
				Title:         "test feature with CI failure",
				Body:          "Test PR body",
				BaseRef:       "main",
				HeadRef:       "feature/test",
				HasDCO:        true,
				CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "failure", "lint errors found")},
				Coverage:      85.0,
				Commits:       []*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")},
			},
			expectedError:  &ValidationError{Type: CIValidation, Message: "Required CI checks are failing"},
			shouldPass:    false,
			errorType:     CIValidation,
		},
		{
			name: "PR fails coverage validation",
			prData: &PullRequestData{
				PRNumber:      192,
				Author:        "testuser",
				Title:         "test feature with low coverage",
				Body:          "Test PR body",
				BaseRef:       "main",
				HeadRef:       "feature/test",
				HasDCO:        true,
				CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "success", "")},
				Coverage:      45.0, // Below threshold
				Commits:       []*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")},
			},
			expectedError:  &ValidationError{Type: CoverageValidation, Message: "Code coverage (45.00%) is below minimum threshold (55.00%)"},
			shouldPass:    false,
			errorType:     CoverageValidation,
		},
		{
			name: "PR has missing check runs",
			prData: &PullRequestData{
				PRNumber:      193,
				Author:        "testuser",
				Title:         "test feature missing security check",
				Body:          "Test PR body",
				BaseRef:       "main",
				HeadRef:       "feature/test",
				HasDCO:        true,
				CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "success", "")}, // Missing security check
				Coverage:      85.0,
				Commits:       []*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")},
			},
			expectedError:  &ValidationError{Type: CIValidation, Message: "Required CI checks are missing"},
			shouldPass:    false,
			errorType:     CIValidation,
		},
		{
			name: "PR with timed out API calls",
			prData: nil, // Will trigger timeout in validation
			expectedError:  &ValidationError{Type: SystemError, Message: "validation timed out after 30 seconds"},
			shouldPass:    false,
			errorType:     SystemError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := NewPRValidationGate(&Config{
				MinCoverage:    55.0,
				RequiredChecks: []string{"ci/lint", "security"},
				Timeout:        30 * time.Second,
			})

			result, err := gate.Validate(context.Background(), tt.prData)

			if tt.shouldPass {
				assert.NoError(t, err, "Expected validation to pass")
				assert.True(t, result.IsValid, "Expected result to be valid")
				assert.NotEmpty(t, result.CheckResults, "Expected check results")
			} else {
				assert.Error(t, err, "Expected validation to fail")
				assert.False(t, result.IsValid, "Expected result to be invalid")
				
				if tt.expectedError != nil {
					var validationErr *ValidationError
					assert.True(t, errors.As(err, &validationErr), "Expected ValidationError type")
					assert.Equal(t, tt.expectedError.Error(), err.Error())
					assert.Equal(t, tt.errorType, validationErr.Type)
				}
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

// Test integration with mock GitHub API
func TestPRValidationGateWithMockGitHub(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/test/repo/pulls/189":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"number": 189,
				"title": "Test PR",
				"user": {"login": "testuser"},
				"head": {"ref": "feature/test"},
				"base": {"ref": "main"},
				"body": "Test PR body"
			}`))
		case "/repos/test/repo/pulls/189/files":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Test with mock server
	mockClient := &MockGitHubClient{}
	
	gate := &PRValidationGate{
		config: &Config{
			MinCoverage:    55.0,
			RequiredChecks: []string{"ci/lint"},
			Timeout:        10 * time.Second,
		},
		githubClient: mockClient,
	}

	// Set up mock expectations
	mockClient.On("GetPullRequest", context.Background(), "test", "repo", 189).
		Return(&github.PullRequest{
			Number: github.Int(189),
			Title:  github.String("Test PR"),
			User:   &github.User{Login: github.String("testuser")},
			Head:   &github.PullRequestBranch{Ref: github.String("feature/test")},
			Base:   &github.PullRequestBranch{Ref: github.String("main")},
			Body:   github.String("Test PR body"),
		}, nil, nil)

	mockClient.On("ListPullRequestCommits", context.Background(), "test", "repo", 189, mock.Anything).
		Return([]*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")}, nil, nil)

	mockClient.On("GetCheckRunsForRef", context.Background(), "test", "repo", "feature/test", mock.Anything).
		Return([]*github.CheckRun{createMockCheckRun("ci/lint", "success", "")}, nil, nil)

	prData := &PullRequestData{
		PRNumber: 189,
		Owner:     "test",
		Repo:      "repo",
	}

	result, err := gate.Validate(context.Background(), prData)
	
	assert.NoError(t, err)
	assert.True(t, result.IsValid)
	assert.Equal(t, 1, len(result.CheckResults))
}

// Benchmark tests
func BenchmarkPRValidationGate(b *testing.B) {
	gate := NewPRValidationGate(&Config{
		MinCoverage:    55.0,
		RequiredChecks: []string{"ci/lint", "security", "integration"},
		Timeout:        30 * time.Second,
	})

	prData := &PullRequestData{
		PRNumber:      189,
		Author:        "testuser",
		Title:         "test feature implementation",
		Body:          "Test PR body",
		BaseRef:       "main",
		HeadRef:       "feature/test",
		HasDCO:        true,
		CheckRuns:     []*github.CheckRun{createMockCheckRun("ci/lint", "success", "")},
		Coverage:      85.0,
		Commits:       []*github.RepositoryCommit{createMockCommit("Signed-off-by: testuser <test@example.com>")},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gate.Validate(context.Background(), prData)
	}
}

// Helper functions
func createMockCheckRun(name, status, conclusion string) *github.CheckRun {
	return &github.CheckRun{
		ID:        github.Int64(1),
		Name:      github.String(name),
		Status:    github.String(status),
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