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
	"net/http"
	"time"
)

// SourceProvider is THE provider seam (story 11.5, arch §5.4/§10.2): every
// byte of provider-specific knowledge in this codebase lives behind it.
// GitHub is the v1 implementation; GitLab/Bitbucket/Gitea drop in as new
// implementations + a ProviderRegistry entry — with ZERO change to the
// repo-sync reconciler, the scm-webhook ingress, or the mirror.
//
// The seam discipline mirrors the §10 runtime shims: the reconciler and the
// ingress code against NormalizedRecord/WebhookEvent shapes only, never
// provider API types, provider header names, or provider payload schemas.
// Adding a provider must never introduce a provider-name branch outside
// pkg/scm and the composition root.
type SourceProvider interface {
	// Name returns the provider name (github, gitlab, gitea).
	Name() string

	// Snapshot fetches the current state of all repository objects from the provider.
	// This returns normalized records in the common shape expected by the reconciler.
	// The reconciler uses this to implement level-triggered idempotent upsert.
	Snapshot(ctx context.Context, repoURL string, options SnapshotOptions) ([]NormalizedRecord, error)

	// VerifyWebhookDelivery authenticates one webhook delivery using the
	// provider's OWN scheme — which header carries the credential and what
	// form it takes is provider knowledge and stays inside the provider
	// (GitHub: X-Hub-Signature-256 HMAC; GitLab: X-Gitlab-Token shared
	// secret). It is called BEFORE any payload parsing (story 11.1 AC4,
	// D8/NFR-SEC8) and must be constant time. False means drop, always.
	VerifyWebhookDelivery(ctx context.Context, headers http.Header, payload []byte, secret string) bool

	// ParseWebhookEvent extracts a normalized event summary from a VERIFIED
	// delivery — the provider's event header and payload schema stay inside
	// the provider. Called only after VerifyWebhookDelivery returned true.
	// An unparseable delivery is an error, not a refusal: the reconcile the
	// delivery triggers is level-triggered and never trusts the payload.
	ParseWebhookEvent(ctx context.Context, headers http.Header, payload []byte) (*WebhookEvent, error)

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

// SourceControlProvider is the pre-11.5 name of SourceProvider, kept as an
// alias so external references to the seam keep compiling. New code uses
// SourceProvider.
type SourceControlProvider = SourceProvider

// WebhookEvent is the provider-agnostic summary of one webhook delivery,
// extracted AFTER verification. It exists for logging/trigger attribution
// only — the mirror is written exclusively from provider snapshots, never
// from webhook payloads (story 11.1 AC2).
type WebhookEvent struct {
	// Type is the normalized event type: issue, pull_request, check_run,
	// artifact, push, ping, unknown. Providers map their native event
	// names onto this set; "unknown" is a valid answer.
	Type string `json:"type"`

	// Action is the event's action verb (opened, closed, ...), or empty.
	Action string `json:"action,omitempty"`

	// DeliveryID is the provider's delivery/request identifier, for
	// correlation in logs. Empty when the provider does not supply one.
	DeliveryID string `json:"delivery_id,omitempty"`
}

// WebhookExtractor is the provider-agnostic seam for reading an inbound
// webhook HTTP request. Webhook receipt needs no API credentials, so it is a
// separate, STATELESS seam from SourceProvider — a GitLab or Bitbucket
// build supplies its own extractor (reading X-Gitlab-Token / etc.) with ZERO
// change to the webhook handler, the same "new impl + config, no reconciler
// rewrite" discipline the ProviderRegistry gives the reconcile loop
// (Story 11.5).
//
// The seam deliberately splits into two calls so the handler can keep the
// verify-before-parse gate (Story 11.1 AC4, D8/NFR-SEC8): Signature is
// header-only and runs BEFORE HMAC verification; Event may inspect the body
// and is called ONLY after the signature verifies.
type WebhookExtractor interface {
	// Signature returns the bare HMAC digest carried by the provider's
	// signature header (GitHub: X-Hub-Signature-256, "sha256=<hex>"). An
	// absent or malformed signature is an error the handler treats as an
	// unverifiable delivery to drop — never an unsigned parse. Reads
	// headers ONLY; it must not touch the body (which is unverified here).
	Signature(header http.Header) (string, error)

	// Event returns a short, provider-normalized event name for logging and
	// trigger decisions. It is called only after the signature has verified,
	// so it may inspect the now-trusted body as a fallback when the
	// provider's event header is absent.
	Event(header http.Header, body []byte) string
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
