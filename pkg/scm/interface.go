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
	"time"
)

// SourceControlProvider defines the interface for source control operations.
// This is the provider seam that enables different source control providers
// to be used interchangeably by the repo-sync reconciler.
type SourceControlProvider interface {
	// Name returns the provider name (github, gitlab, gitea).
	Name() string

	// Snapshot fetches the current state of all repository objects from the provider.
	// This returns normalized records in the common shape expected by the reconciler.
	// The reconciler uses this to implement level-triggered idempotent upsert.
	Snapshot(ctx context.Context, repoURL string, options SnapshotOptions) ([]NormalizedRecord, error)

	// ValidateWebhook verifies the HMAC signature of a webhook delivery.
	// Returns true if the signature is valid, false otherwise.
	// This is called BEFORE any webhook payload parsing (AC4 from Story 11.1).
	ValidateWebhook(ctx context.Context, signature string, secret string, payload []byte) bool

	// CreateComment creates a comment on an issue or PR.
	// Used for outbound reflection when reflectOutbound is enabled.
	// Returns the created comment ID.
	CreateComment(ctx context.Context, repoURL string, kind string, externalID string, comment string) (string, error)

	// CreateStatus creates a status on a commit or PR.
	// Used for outbound reflection when reflectOutbound is enabled.
	CreateStatus(ctx context.Context, repoURL string, sha string, status Status) error

	// GetRepo fetches repository information.
	GetRepo(ctx context.Context, repoURL string) (*Repository, error)
}

// SnapshotOptions contains options for snapshot operations.
type SnapshotOptions struct {
	// Branch specifies a specific branch to snapshot. If empty, snapshots all.
	Branch string

	// Since specifies the timestamp from which to fetch changes.
	// If zero, fetches all changes.
	Since time.Time

	// Types specifies which record types to fetch.
	// If empty, fetches all types.
	Types []RecordType
}

// RecordType enumerates the types of source control records.
type RecordType string

const (
	RecordTypeIssue    RecordType = "issue"
	RecordTypePR       RecordType = "pr"
	RecordTypeCheckRun RecordType = "check_run"
	RecordTypeArtifact RecordType = "artifact"
	RecordTypeRelease  RecordType = "release"
)

// NormalizedRecord represents a normalized source control record.
// This is the common shape that all providers must map their API responses to.
// The reconciler only sees this shape, never provider-specific types.
type NormalizedRecord struct {
	// Kind is the record type (issue, pr, check_run, etc.).
	Kind RecordType `json:"kind"`

	// ExternalID is the provider's unique identifier for this record.
	ExternalID string `json:"external_id"`

	// State is the current state of the record (open, closed, success, etc.).
	State string `json:"state"`

	// Title is the title/summary of the record.
	Title string `json:"title"`

	// Body is the detailed content of the record.
	Body string `json:"body,omitempty"`

	// URL is the web URL for this record.
	URL string `json:"url,omitempty"`

	// Author is the username of the user who created this record.
	Actor string `json:"actor"`

	// CreatedAt is when the record was created.
	CreatedAt time.Time `json:"created_at,omitempty"`

	// UpdatedAt is when the record was last updated.
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// Number is the sequential number (for issues/PRs).
	Number int `json:"number,omitempty"`

	// Assignees are the users assigned to this record.
	Assignees []string `json:"assignees,omitempty"`

	// Labels are the labels applied to this record.
	Labels []string `json:"labels,omitempty"`

	// PR-specific fields
	HeadRef string `json:"head_ref,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	Merged  bool   `json:"merged,omitempty"`

	// CheckRun-specific fields
	Conclusion string    `json:"conclusion,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`

	// Artifact-specific fields
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Size      int64     `json:"size,omitempty"`

	// Provider-specific raw data (for debugging)
	Raw map[string]interface{} `json:"raw,omitempty"`
}

// Status represents a commit or PR status.
type Status struct {
	State       string    // pending, success, failure, error
	Context     string    // The status context (e.g., "ci/travis-ci")
	TargetURL   string    // URL for details about the status
	Description string    // Short description
	CreatedAt   time.Time // When the status was created
	UpdatedAt   time.Time // When the status was last updated
}

// Repository represents a source control repository.
type Repository struct {
	Name          string
	FullName      string
	CloneURL      string
	DefaultBranch string
	Private       bool
	Description   string
	Language      string
	StarCount     int
	LastPushedAt  time.Time
}

// ProviderCredentials holds resolved provider credentials.
type ProviderCredentials struct {
	Token     string
	TokenType string // "pat", "oauth", etc.
	ExpiresAt time.Time
}

// ProviderError represents provider-specific errors.
type ProviderError struct {
	HTTPCode int
	Message  string
	Details  map[string]interface{}
}

func (e *ProviderError) Error() string {
	return e.Message
}

// IsNotFound returns true if the error indicates a resource was not found.
func (e *ProviderError) IsNotFound() bool {
	return e.HTTPCode == 404
}

// IsForbidden returns true if the error indicates an authorization failure.
func (e *ProviderError) IsForbidden() bool {
	return e.HTTPCode == 403
}
