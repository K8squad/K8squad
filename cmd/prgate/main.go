package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/go-github/v58/github"
	"github.com/K8squad/K8squad/pkg/prgate"
)

func main() {
	// Example configuration
	config := &prgate.Config{
		MinCoverage:    55.0, // Minimum 55% code coverage
		RequiredChecks: []string{"ci/lint", "security", "integration"},
		Timeout:        30 * time.Second,
		AllowWarnings:  false,
	}

	// Create validation gate
	gate, err := prgate.NewPRValidationGate(config)
	if err != nil {
		log.Fatalf("Failed to create PR validation gate: %v", err)
	}

	// Example PR data
	prData := &prgate.PullRequestData{
		PRNumber: 189,
		Author:    "testuser",
		Title:     "Implement new authentication system",
		Body:      "This PR implements a new authentication system with OAuth2 support.",
		BaseRef:   "main",
		HeadRef:   "feature/auth-system",
		HasDCO:    true,
		Coverage:  85.0, // 85% coverage
		CreatedAt: time.Now(),
	}

	// Mock CI check runs
	prData.CheckRuns = []*github.CheckRun{
		{
			ID:        github.Int64(1),
			Name:      github.String("ci/lint"),
			Status:    github.String("completed"),
			Conclusion: github.String("success"),
			Output: &github.CheckRunOutput{
				Summary: github.String("Linting passed"),
			},
		},
		{
			ID:        github.Int64(2),
			Name:      github.String("security"),
			Status:    github.String("completed"),
			Conclusion: github.String("success"),
			Output: &github.CheckRunOutput{
				Summary: github.String("Security scan passed"),
			},
		},
		{
			ID:        github.Int64(3),
			Name:      github.String("integration"),
			Status:    github.String("completed"),
			Conclusion: github.String("success"),
			Output: &github.CheckRunOutput{
				Summary: github.String("Integration tests passed"),
			},
		},
	}

	// Mock commits with DCO signatures
	prData.Commits = []*github.RepositoryCommit{
		{
			Commit: &github.Commit{
				Message: github.String("feat: Implement OAuth2 authentication\n\nSigned-off-by: testuser <test@example.com>"),
			},
		},
		{
			Commit: &github.Commit{
				Message: github.String("fix: Resolve authentication timeout issues\n\nSigned-off-by: testuser <test@example.com>"),
			},
		},
	}

	// Validate the PR
	ctx := context.Background()
	result, err := gate.Validate(ctx, prData)
	if err != nil {
		log.Fatalf("Failed to validate PR: %v", err)
	}

	// Display results
	fmt.Printf("PR Validation Result for #%d\n", prData.PRNumber)
	fmt.Printf("===============================\n")
	fmt.Printf("Valid: %v\n", result.IsValid)
	fmt.Printf("Duration: %v\n", result.Duration)
	fmt.Printf("\n%s\n", result.Summary)

	if !result.IsValid {
		fmt.Println("\nFailed Checks:")
		for _, check := range result.CheckResults {
			if check.Status == "fail" {
				fmt.Printf("  ❌ %s: %s\n", check.Name, check.Message)
			}
		}
	}
}