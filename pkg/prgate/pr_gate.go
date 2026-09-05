package prgate

import (
	"context"
	"fmt"
	"strings"
	"time"
)

import (
	"github.com/google/go-github/v58/github"
)

// ValidationError represents different types of validation errors
type ValidationErrorType int

const (
	DCOValidation ValidationErrorType = iota
	CIValidation
	CoverageValidation
	SecurityValidation
	SystemError
)

// ValidationError is the error type for PR validation failures
type ValidationError struct {
	Type    ValidationErrorType
	Message string
	Field   string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s validation failed for %s: %s", e.Type.String(), e.Field, e.Message)
	}
	return fmt.Sprintf("%s validation failed: %s", e.Type.String(), e.Message)
}

func (e ValidationErrorType) String() string {
	return [...]string{
		"DCO",
		"CI",
		"Coverage",
		"Security",
		"System",
	}[e]
}

// CheckResult represents the result of a specific validation check
type CheckResult struct {
	Name     string
	Status   string // "pass", "fail", "warning"
	Message  string
	Duration time.Duration
}

// ValidationResult represents the overall result of PR validation
type ValidationResult struct {
	IsValid bool
	CheckResults []CheckResult
	Summary string
	Duration time.Duration
}

// PullRequestData contains all necessary data for PR validation
type PullRequestData struct {
	PRNumber   int
	Author     string
	Title      string
	Body       string
	BaseRef    string
	HeadRef    string
	HasDCO     bool
	CheckRuns  []*github.CheckRun
	CheckResults []CheckResult
	Coverage   float64 // Percentage
	Commits    []*github.RepositoryCommit
	CreatedAt  time.Time
}

// Config contains configuration for the PR validation gate
type Config struct {
	MinCoverage    float64
	RequiredChecks  []string
	Timeout        time.Duration
	AllowWarnings  bool
}

// PRValidationGate is the main implementation of PR validation
type PRValidationGate struct {
	config       *Config
	githubClient GitHubClient
}

// GitHubClient defines the interface for GitHub API operations
type GitHubClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, *github.Response, error)
	ListPullRequestCommits(ctx context.Context, owner, repo string, number int, opts *github.ListOptions) ([]*github.RepositoryCommit, *github.Response, error)
	GetCheckRunsForRef(ctx context.Context, owner, repo, ref string, opts *github.ListCheckRunsOptions) ([]*github.CheckRun, *github.Response, error)
}

// NewPRValidationGate creates a new PR validation gate instance
func NewPRValidationGate(config *Config) (*PRValidationGate, error) {
	// Validate configuration
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &PRValidationGate{
		config: config,
	}, nil
}

// Validate performs comprehensive PR validation
func (g *PRValidationGate) Validate(ctx context.Context, prData *PullRequestData) (*ValidationResult, error) {
	startTime := time.Now()
	
	if prData == nil {
		return nil, &ValidationError{
			Type:    SystemError,
			Message: "PR data is required for validation",
		}
	}

	// Create validation result
	result := &ValidationResult{
		CheckResults: make([]CheckResult, 0),
	}

	// Perform all validation checks
	checks := []CheckResult{
		g.validateDCO(prData),
		g.validateCIStatus(prData),
		g.validateCoverage(prData),
		g.validateSecurity(prData),
	}

	// Add all check results
	result.CheckResults = append(result.CheckResults, checks...)
	
	// Determine overall validity
	result.IsValid = true
	for _, check := range checks {
		if check.Status == "fail" {
			result.IsValid = false
			break
		}
		if check.Status == "warning" && !g.config.AllowWarnings {
			result.IsValid = false
			break
		}
	}

	// Generate summary
	result.Summary = g.generateSummary(result)
	result.Duration = time.Since(startTime)

	return result, nil
}

// validateDCO validates Developer Certificate of Origin compliance
func (g *PRValidationGate) validateDCO(prData *PullRequestData) CheckResult {
	startTime := time.Now()
	
	if prData.HasDCO {
		return CheckResult{
			Name:     "DCO Compliance",
			Status:   "pass",
			Message:  "All commits are properly signed off",
			Duration: time.Since(startTime),
		}
	}

	// Detailed DCO validation
	hasDCO := true
	for _, commit := range prData.Commits {
		message := commit.GetCommit().GetMessage()
		if !strings.Contains(message, "Signed-off-by:") {
			hasDCO = false
			break
		}
	}

	if hasDCO {
		return CheckResult{
			Name:     "DCO Compliance",
			Status:   "pass",
			Message:  "All commits are properly signed off",
			Duration: time.Since(startTime),
		}
	}

	return CheckResult{
		Name:     "DCO Compliance",
		Status:   "fail",
		Message:  "PR commits are not properly signed off",
		Duration: time.Since(startTime),
	}
}

// validateCIStatus validates CI/CD pipeline status
func (g *PRValidationGate) validateCIStatus(prData *PullRequestData) CheckResult {
	startTime := time.Now()
	
	// Check if all required checks are present and passing
	requiredCheckMap := make(map[string]bool)
	for _, required := range g.config.RequiredChecks {
		requiredCheckMap[required] = true
	}

	// Track found checks
	foundChecks := make(map[string]bool)
	
	// Validate each check run
	allPassing := true
	for _, checkRun := range prData.CheckRuns {
		checkName := checkRun.GetName()
		
		// Only check required checks
		if requiredCheckMap[checkName] {
			foundChecks[checkName] = true
			
			status := checkRun.GetStatus()
			conclusion := checkRun.GetConclusion()
			
			// Determine check status
			checkStatus := "pass"
			switch status {
			case "completed":
				switch conclusion {
				case "failure", "cancelled":
					checkStatus = "fail"
					allPassing = false
				case "neutral", "timed_out":
					checkStatus = "warning"
				}
			case "failure":
				checkStatus = "fail"
				allPassing = false
			}
			
			// Add individual check result
			prData.CheckResults = append(prData.CheckResults, CheckResult{
				Name:     checkName,
				Status:   checkStatus,
				Message:  checkRun.GetOutput().GetSummary(),
				Duration: time.Since(startTime),
			})
		}
	}

	// Check if all required checks were found
	allFound := true
	for required := range requiredCheckMap {
		if !foundChecks[required] {
			allFound = false
			break
		}
	}

	if !allFound {
		return CheckResult{
			Name:     "CI Status",
			Status:   "fail",
			Message:  "Required CI checks are missing",
			Duration: time.Since(startTime),
		}
	}

	if allPassing {
		return CheckResult{
			Name:     "CI Status",
			Status:   "pass",
			Message:  "All required CI checks are passing",
			Duration: time.Since(startTime),
		}
	}

	return CheckResult{
		Name:     "CI Status",
		Status:   "fail",
		Message:  "Required CI checks are failing",
		Duration: time.Since(startTime),
	}
}

// validateCoverage validates code coverage requirements
func (g *PRValidationGate) validateCoverage(prData *PullRequestData) CheckResult {
	startTime := time.Now()
	
	if prData.Coverage >= g.config.MinCoverage {
		return CheckResult{
			Name:     "Code Coverage",
			Status:   "pass",
			Message:  fmt.Sprintf("Code coverage (%.2f%%) meets minimum threshold (%.2f%%)", prData.Coverage, g.config.MinCoverage),
			Duration: time.Since(startTime),
		}
	}

	return CheckResult{
		Name:     "Code Coverage",
		Status:   "fail",
		Message:  fmt.Sprintf("Code coverage (%.2f%%) is below minimum threshold (%.2f%%)", prData.Coverage, g.config.MinCoverage),
		Duration: time.Since(startTime),
	}
}

// validateSecurity validates security-related checks
func (g *PRValidationGate) validateSecurity(prData *PullRequestData) CheckResult {
	startTime := time.Now()
	
	// Look for security-specific checks
	hasSecurityCheck := false
	securityPassed := true
	
	for _, checkRun := range prData.CheckRuns {
		checkName := checkRun.GetName()
		
		// Check for security-related check names
		if strings.Contains(strings.ToLower(checkName), "security") ||
			strings.Contains(strings.ToLower(checkName), "vuln") ||
			strings.Contains(strings.ToLower(checkName), "audit") {
			
			hasSecurityCheck = true
			
			status := checkRun.GetStatus()
			conclusion := checkRun.GetConclusion()
			
			if status == "completed" && (conclusion == "failure" || conclusion == "cancelled") {
				securityPassed = false
			}
		}
	}
	
	if hasSecurityCheck {
		if securityPassed {
			return CheckResult{
				Name:     "Security Validation",
				Status:   "pass",
				Message:  "Security checks are passing",
				Duration: time.Since(startTime),
			}
		}
		
		return CheckResult{
			Name:     "Security Validation",
			Status:   "fail",
			Message:  "Security checks are failing",
			Duration: time.Since(startTime),
		}
	}
	
	// No security checks found, treat as warning
	return CheckResult{
		Name:     "Security Validation",
		Status:   "warning",
		Message:  "No security checks found",
		Duration: time.Since(startTime),
	}
}

// generateSummary generates a human-readable summary of validation results
func (g *PRValidationGate) generateSummary(result *ValidationResult) string {
	var summary strings.Builder
	
	summary.WriteString("PR Validation Summary\n")
	summary.WriteString("=====================\n")
	
	if result.IsValid {
		summary.WriteString("✅ PR validation PASSED\n")
	} else {
		summary.WriteString("❌ PR validation FAILED\n")
	}
	
	fmt.Fprintf(&summary, "Duration: %v\n", result.Duration)
	fmt.Fprintf(&summary, "Checks performed: %d\n", len(result.CheckResults))
	
	for _, check := range result.CheckResults {
		statusIcon := "✅"
		switch check.Status {
		case "fail":
			statusIcon = "❌"
		case "warning":
			statusIcon = "⚠️"
		}
		
		fmt.Fprintf(&summary, "%s %s: %s\n", statusIcon, check.Name, check.Message)
	}
	
	return summary.String()
}

// validateConfig validates the configuration for the PR validation gate
func validateConfig(config *Config) error {
	if config.MinCoverage < 0 || config.MinCoverage > 100 {
		return fmt.Errorf("invalid coverage threshold: must be between 0 and 100")
	}
	
	if len(config.RequiredChecks) == 0 {
		return fmt.Errorf("at least one required check must be specified")
	}
	
	if config.Timeout <= 0 {
		return fmt.Errorf("timeout must be greater than zero")
	}
	
	return nil
}

// GetRequiredChecks returns the list of required checks
func (g *PRValidationGate) GetRequiredChecks() []string {
	return g.config.RequiredChecks
}

// GetMinCoverage returns the minimum coverage requirement
func (g *PRValidationGate) GetMinCoverage() float64 {
	return g.config.MinCoverage
}

// SetGitHubClient sets the GitHub client for testing
func (g *PRValidationGate) SetGitHubClient(client GitHubClient) {
	g.githubClient = client
}

// ValidateWithEnhancedErrors validates PR with enhanced error handling
func (g *PRValidationGate) ValidateWithEnhancedErrors(ctx context.Context, prData *PullRequestData, enhancer *ErrorEnhancer) (*ValidationResult, error) {
	result, err := g.Validate(ctx, prData)
	if err != nil {
		if enhancer != nil {
			context := map[string]interface{}{
				"pr_number":   prData.PRNumber,
				"author":      prData.Author,
				"title":       prData.Title,
				"base_ref":    prData.BaseRef,
				"head_ref":    prData.HeadRef,
				"coverage":    prData.Coverage,
				"has_dco":     prData.HasDCO,
				"check_count": len(prData.CheckRuns),
				"commit_count": len(prData.Commits),
				"timestamp":   time.Now(),
			}
			return result, enhancer.EnhanceError(err, context)
		}
	}
	return result, err
}