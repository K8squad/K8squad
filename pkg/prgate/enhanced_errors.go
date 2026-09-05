package prgate

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// EnhancedError represents an enhanced error message with detailed context
type EnhancedError struct {
	Type         string                 `json:"type"`
	Message      string                 `json:"message"`
	Details      map[string]interface{} `json:"details"`
	Suggestions  []string               `json:"suggestions"`
	Stack        string                 `json:"stack,omitempty"`
	Timestamp    time.Time              `json:"timestamp"`
	ErrorID      string                 `json:"error_id"`
	Context      map[string]interface{} `json:"context"`
}

// Error implements the error interface
func (e *EnhancedError) Error() string {
	return e.Message
}

// JSON returns the error as JSON
func (e *EnhancedError) JSON() string {
	data, _ := json.MarshalIndent(e, "", "  ")
	return string(data)
}

// String returns a formatted error message
func (e *EnhancedError) String() string {
	var builder strings.Builder
	
	fmt.Fprintf(&builder, "[%s] %s\n", e.Type, e.Message)

	if len(e.Details) > 0 {
		builder.WriteString("Details:\n")
		for key, value := range e.Details {
			fmt.Fprintf(&builder, "  %s: %v\n", key, value)
		}
	}

	if len(e.Suggestions) > 0 {
		builder.WriteString("Suggestions:\n")
		for i, suggestion := range e.Suggestions {
			fmt.Fprintf(&builder, "  %d. %s\n", i+1, suggestion)
		}
	}

	if e.ErrorID != "" {
		fmt.Fprintf(&builder, "Error ID: %s\n", e.ErrorID)
	}
	
	if e.Stack != "" {
		builder.WriteString("Stack Trace:\n")
		builder.WriteString(e.Stack)
	}
	
	return builder.String()
}

// ErrorEnhancer provides enhanced error handling with detailed context
type ErrorEnhancer struct {
	errorTemplates map[string]ErrorTemplate
	logger        Logger
}

// ErrorTemplate defines a template for error messages
type ErrorTemplate struct {
	Type        string
	Message     string
	Details     []string
	Suggestions []string
	Pattern     string
}

// Logger interface for logging errors
type Logger interface {
	Errorf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Debugf(format string, args ...interface{})
}

// NewErrorEnhancer creates a new error enhancer
func NewErrorEnhancer(logger Logger) *ErrorEnhancer {
	enhancer := &ErrorEnhancer{
		errorTemplates: make(map[string]ErrorTemplate),
		logger:        logger,
	}
	
	// Initialize default templates
	enhancer.initializeDefaultTemplates()
	
	return enhancer
}

// initializeDefaultTemplates initializes default error templates
func (e *ErrorEnhancer) initializeDefaultTemplates() {
	// DCO validation templates
	e.errorTemplates["dco_missing"] = ErrorTemplate{
		Type:    "DCO_VALIDATION_ERROR",
		Message: "Developer Certificate of Origin validation failed",
		Details: []string{
			"Missing Signed-off-by trailer in commit messages",
			"Please ensure all commits are properly signed",
		},
		Suggestions: []string{
			"Use 'git commit -s' to sign commits",
			"Add 'Signed-off-by: Name <email>' to commit messages",
			"Review commit history with 'git log --show-signature'",
		},
		Pattern: `(?i)(?!.*signed-off-by)`,
	}
	
	e.errorTemplates["dco_invalid"] = ErrorTemplate{
		Type:    "DCO_FORMAT_ERROR",
		Message: "Invalid DCO format detected",
		Details: []string{
			"Malformed Signed-off-by trailer in commit messages",
			"Invalid email format in signature",
		},
		Suggestions: []string{
			"Use format: 'Signed-off-by: Name <email>'",
			"Ensure email address is valid",
			"Check for extra spaces or special characters",
		},
		Pattern: `(?i)(signed-off-by:[^<]*<[^>]*>)(.*invalid.*)`,
	}
	
	// CI validation templates
	e.errorTemplates["ci_missing"] = ErrorTemplate{
		Type:    "CI_VALIDATION_ERROR",
		Message: "Required CI checks are missing",
		Details: []string{
			"Some required CI checks did not run",
			"Check may not be configured for this PR",
		},
		Suggestions: []string{
			"Verify CI check configuration in .github/workflows/",
			"Ensure check names match required patterns",
			"Check if the check is configured to run on PRs",
		},
		Pattern: `(?i)(required.*check.*missing|check.*not.*found)`,
	}
	
	e.errorTemplates["ci_failure"] = ErrorTemplate{
		Type:    "CI_VALIDATION_ERROR",
		Message: "CI checks are failing",
		Details: []string{
			"One or more CI checks have failed",
			"Please review the check outputs for details",
		},
		Suggestions: []string{
			"Check the specific CI check logs for failure details",
			"Fix any linting or compilation errors",
			"Ensure all tests are passing locally",
		},
		Pattern: `(?i)(check.*fail|fail.*check|status.*fail)`,
	}
	
	// Coverage validation templates
	e.errorTemplates["coverage_low"] = ErrorTemplate{
		Type:    "COVERAGE_VALIDATION_ERROR",
		Message: "Code coverage is below required threshold",
		Details: []string{
			"Current coverage does not meet minimum requirements",
			"Additional test coverage needed",
		},
		Suggestions: []string{
			"Add unit tests for uncovered code paths",
			"Focus on critical business logic first",
			"Consider integration tests for complex scenarios",
			"Review existing tests for improvement opportunities",
		},
		Pattern: `(?i)(coverage.*low|below.*threshold|insufficient.*coverage)`,
	}
	
	// Security validation templates
	e.errorTemplates["security_failure"] = ErrorTemplate{
		Type:    "SECURITY_VALIDATION_ERROR",
		Message: "Security checks are failing",
		Details: []string{
			"Vulnerabilities or security issues detected",
			"Review security scan results for details",
		},
		Suggestions: []string{
			"Review security scan results for specific issues",
			"Update dependencies to latest secure versions",
			"Address any high or critical vulnerabilities",
			"Consider code changes to mitigate security risks",
		},
		Pattern: `(?i)(security.*fail|vulnerability|security.*issue)`,
	}
	
	// System error templates
	e.errorTemplates["timeout"] = ErrorTemplate{
		Type:    "SYSTEM_ERROR",
		Message: "Validation operation timed out",
		Details: []string{
			"Validation took longer than expected",
			"Possible network or performance issues",
		},
		Suggestions: []string{
			"Increase timeout configuration",
			"Check network connectivity to GitHub",
			"Consider optimizing validation logic",
			"Monitor system performance",
		},
		Pattern: `(?i)(timeout|time.*out)`,
	}
	
	e.errorTemplates["api_error"] = ErrorTemplate{
		Type:    "API_ERROR",
		Message: "GitHub API error occurred",
		Details: []string{
			"Error communicating with GitHub API",
			"Check API rate limits and authentication",
		},
		Suggestions: []string{
			"Verify GitHub token permissions",
			"Check API rate limits",
			"Ensure network connectivity",
			"Try again later if rate limited",
		},
		Pattern: `(?i)(api.*error|github.*error|rate.*limit)`,
	}
	
	e.errorTemplates["config_error"] = ErrorTemplate{
		Type:    "CONFIGURATION_ERROR",
		Message: "Invalid configuration detected",
		Details: []string{
			"Configuration validation failed",
			"Check configuration parameters",
		},
		Suggestions: []string{
			"Review configuration file format",
			"Check parameter ranges and types",
			"Validate required fields are present",
			"Use example configuration as reference",
		},
		Pattern: `(?i)(config.*error|invalid.*config)`,
	}
}

// EnhanceError enhances an error with detailed context and suggestions
func (e *ErrorEnhancer) EnhanceError(err error, context map[string]interface{}) *EnhancedError {
	startTime := time.Now()
	
	// Create enhanced error
	enhanced := &EnhancedError{
		Details:     make(map[string]interface{}),
		Suggestions:  make([]string, 0),
		Timestamp:   startTime,
		ErrorID:     generateErrorID(),
		Context:     context,
	}
	
	// Determine error type and apply template
	if validationErr, ok := err.(*ValidationError); ok {
		enhanced.Type = validationErr.Type.String()
		enhanced.Message = e.formatValidationMessage(validationErr)
		e.applyValidationTemplate(enhanced, validationErr)
	} else {
		enhanced.Type = "UNKNOWN_ERROR"
		enhanced.Message = err.Error()
		e.applyGenericTemplate(enhanced, err)
	}
	
	// Add context details
	for key, value := range context {
		enhanced.Details[key] = value
	}
	
	// Add stack trace if available
	if e.logger != nil {
		e.logger.Errorf("Enhanced error: %s", enhanced.String())
		e.logger.Debugf("Error context: %v", context)
	}
	
	return enhanced
}

// formatValidationMessage formats a validation error message with details
func (e *ErrorEnhancer) formatValidationMessage(err *ValidationError) string {
	switch err.Type {
	case DCOValidation:
		return "DCO validation failed: " + err.Message
	case CIValidation:
		return "CI validation failed: " + err.Message
	case CoverageValidation:
		return "Coverage validation failed: " + err.Message
	case SecurityValidation:
		return "Security validation failed: " + err.Message
	case SystemError:
		return "System error: " + err.Message
	default:
		return "Validation error: " + err.Message
	}
}

// applyValidationTemplate applies a template to a validation error
func (e *ErrorEnhancer) applyValidationTemplate(enhanced *EnhancedError, err *ValidationError) {
	// Find matching template
	template, found := e.findMatchingTemplate(err.Message)
	if found {
		enhanced.Type = template.Type
		enhanced.Suggestions = template.Suggestions
		
		// Add details from template
		for _, detail := range template.Details {
			enhanced.Details["template_detail"] = detail
		}
	}
	
	// Add validation-specific details
	enhanced.Details["validation_type"] = err.Type.String()
	enhanced.Details["field"] = err.Field
	enhanced.Details["original_message"] = err.Message
}

// applyGenericTemplate applies a template to a generic error
func (e *ErrorEnhancer) applyGenericTemplate(enhanced *EnhancedError, err error) {
	// Find matching template
	template, found := e.findMatchingTemplate(err.Error())
	if found {
		enhanced.Type = template.Type
		enhanced.Suggestions = template.Suggestions
		
		// Add details from template
		for _, detail := range template.Details {
			enhanced.Details["template_detail"] = detail
		}
	}
	
	// Add error-specific details
	enhanced.Details["error_type"] = reflect.TypeOf(err).String()
	enhanced.Details["original_error"] = err.Error()
}

// findMatchingTemplate finds a matching error template
func (e *ErrorEnhancer) findMatchingTemplate(message string) (ErrorTemplate, bool) {
	// Check each template
	for _, template := range e.errorTemplates {
		if template.Pattern != "" {
			matched, _ := regexp.MatchString(template.Pattern, message)
			if matched {
				return template, true
			}
		}
	}
	
	// Default template
	return ErrorTemplate{
		Type:    "UNKNOWN_ERROR",
		Message: "Unknown error occurred",
		Suggestions: []string{
			"Check the error message for details",
			"Review the configuration and try again",
			"Contact support if the issue persists",
		},
	}, false
}

// ValidateAndEnhance validates input and enhances any errors
func (e *ErrorEnhancer) ValidateAndEnhance(validations []func() error, context map[string]interface{}) error {
	for _, validation := range validations {
		err := validation()
		if err != nil {
			enhanced := e.EnhanceError(err, context)
			return enhanced
		}
	}
	return nil
}

// ValidationErrorEnhancer wraps the PR validation gate with enhanced error handling
type ValidationErrorEnhancer struct {
	gate      *PRValidationGate
	enhancer  *ErrorEnhancer
	enabled   bool
}

// NewValidationErrorEnhancer creates a new validation error enhancer
func NewValidationErrorEnhancer(gate *PRValidationGate, enhancer *ErrorEnhancer, enabled bool) *ValidationErrorEnhancer {
	return &ValidationErrorEnhancer{
		gate:     gate,
		enhancer: enhancer,
		enabled:  enabled,
	}
}

// Validate wraps the Validate method with enhanced error handling
func (e *ValidationErrorEnhancer) Validate(ctx context.Context, prData *PullRequestData) (*ValidationResult, error) {
	if !e.enabled {
		return e.gate.Validate(ctx, prData)
	}
	
	// Prepare context for error enhancement
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
	
	// Validate and enhance errors
	result, err := e.gate.Validate(ctx, prData)
	if err != nil {
		enhancedErr := e.enhancer.EnhanceError(err, context)
		return result, enhancedErr
	}
	
	return result, nil
}

// generateErrorID generates a unique error ID
func generateErrorID() string {
	return fmt.Sprintf("ERR-%d-%s", time.Now().UnixNano(), generateRandomString(8))
}

// generateRandomString generates a random string
func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var result strings.Builder
	
	for i := 0; i < length; i++ {
		result.WriteByte(charset[time.Now().UnixNano()%int64(len(charset))])
	}
	
	return result.String()
}

// Helper functions for specific error scenarios

// CreateDCOValidationError creates a DCO validation error with enhanced context
func CreateDCOValidationError(prNumber int, author string, commits []string) *EnhancedError {
	enhancer := NewErrorEnhancer(nil)
	
	context := map[string]interface{}{
		"pr_number": prNumber,
		"author":    author,
		"commit_count": len(commits),
		"commit_examples": commits,
	}
	
	err := &ValidationError{
		Type:    DCOValidation,
		Message: "PR commits are not properly signed off",
		Field:   "commits",
	}
	
	return enhancer.EnhanceError(err, context)
}

// CreateCIValidationError creates a CI validation error with enhanced context
func CreateCIValidationError(prNumber int, author string, missingChecks []string, failingChecks []string) *EnhancedError {
	enhancer := NewErrorEnhancer(nil)
	
	context := map[string]interface{}{
		"pr_number":      prNumber,
		"author":         author,
		"missing_checks": missingChecks,
		"failing_checks": failingChecks,
	}
	
	err := &ValidationError{
		Type:    CIValidation,
		Message: "Required CI checks are missing or failing",
		Field:   "check_runs",
	}
	
	return enhancer.EnhanceError(err, context)
}

// CreateCoverageValidationError creates a coverage validation error with enhanced context
func CreateCoverageValidationError(prNumber int, author string, actualCoverage float64, requiredCoverage float64) *EnhancedError {
	enhancer := NewErrorEnhancer(nil)
	
	context := map[string]interface{}{
		"pr_number":        prNumber,
		"author":           author,
		"actual_coverage":  actualCoverage,
		"required_coverage": requiredCoverage,
		"coverage_gap":     actualCoverage - requiredCoverage,
	}
	
	err := &ValidationError{
		Type:    CoverageValidation,
		Message: fmt.Sprintf("Code coverage (%.2f%%) is below minimum threshold (%.2f%%)", actualCoverage, requiredCoverage),
		Field:   "coverage",
	}
	
	return enhancer.EnhanceError(err, context)
}

// CreateSystemError creates a system error with enhanced context
func CreateSystemError(prNumber int, errorType string, originalError error) *EnhancedError {
	enhancer := NewErrorEnhancer(nil)
	
	context := map[string]interface{}{
		"pr_number":     prNumber,
		"error_type":    errorType,
		"original_error": originalError.Error(),
		"timestamp":     time.Now(),
	}
	
	err := &ValidationError{
		Type:    SystemError,
		Message: fmt.Sprintf("System error: %s", originalError.Error()),
		Field:   "system",
	}
	
	return enhancer.EnhanceError(err, context)
}