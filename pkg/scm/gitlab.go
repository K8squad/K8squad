/*
Copyright 2026 The K8squad Authors.

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
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/http"
)

// GitLabProvider is the story-11.5 drop-in proof for the SourceProvider
// seam: a second provider with a DIFFERENT webhook scheme (GitLab
// authenticates deliveries with the X-Gitlab-Token shared secret, not an
// HMAC signature header) and a different event header/payload schema,
// wired with nothing but an implementation and a ProviderRegistry entry —
// zero change to the reconciler, the scm-webhook ingress, or the mirror.
//
// The CONTROL-plane paths (webhook verification + event parsing) are fully
// implemented, so a Project with spec.repo.sync.provider=gitlab gets a
// working fast path today. The DATA-plane paths (Snapshot, comments,
// statuses, repo info) fail closed with a structured not-implemented
// ProviderError: the reconciler surfaces it on SyncReady instead of
// silently mirroring nothing. Filling them in is additive work inside this
// file alone.
type GitLabProvider struct{}

// NewGitLabProvider builds the GitLab provider. It takes no API client:
// no data-plane call exists to make yet (see the type comment).
func NewGitLabProvider() (*GitLabProvider, error) {
	return &GitLabProvider{}, nil
}

// gitlabNotImplemented is the uniform fail-closed answer for the
// data-plane methods: 501-style ProviderError, never a silent no-op.
func gitlabNotImplemented(op string) *ProviderError {
	return &ProviderError{
		HTTPCode: http.StatusNotImplemented,
		Message:  fmt.Sprintf("gitlab provider: %s is not implemented yet (story 11.5 landed the seam; GitLab data plane is follow-up work)", op),
	}
}

// Name returns "gitlab".
func (p *GitLabProvider) Name() string { return "gitlab" }

// gitlabTokenHeader is GitLab's webhook authentication header: the
// delivery carries the shared secret token itself in plain text (over
// TLS), and the receiver compares it against the configured secret.
const gitlabTokenHeader = "X-Gitlab-Token"

// gitlabEventHeader carries GitLab's event type (Issue Hook, Merge Request
// Hook, Pipeline Hook, ...).
const gitlabEventHeader = "X-Gitlab-Event"

// VerifyWebhookDelivery authenticates a GitLab webhook delivery: the
// X-Gitlab-Token header must equal the configured secret. Comparison is
// constant time (hmac.Equal) even though this is a shared-secret scheme —
// the webhook verifier is on the attacker-timing path regardless of
// provider. Absent header or empty secret verifies false; the caller
// drops the delivery (story 11.1 AC4 applies to every provider).
func (p *GitLabProvider) VerifyWebhookDelivery(_ context.Context, headers http.Header, _ []byte, secret string) bool {
	if headers == nil || secret == "" {
		return false
	}
	token := headers.Get(gitlabTokenHeader)
	if token == "" {
		return false
	}
	return hmac.Equal([]byte(token), []byte(secret))
}

// gitlabEventTypes maps GitLab's native event names onto the normalized
// WebhookEvent.Type set — the provider-faithful names never leave the
// provider (seam discipline, story 11.5).
var gitlabEventTypes = map[string]string{
	"Issue Hook":                 "issue",
	"Merge Request Hook":         "pull_request",
	"Pipeline Hook":              "check_run",
	"Job Hook":                   "check_run",
	"Push Hook":                  "push",
	"Tag Push Hook":              "push",
	"Note Hook":                  "comment",
	"Wiki Page Hook":             "wiki",
	"Release Hook":               "artifact",
	"Deployment Hook":            "deployment",
	"Feature Flag Hook":          "feature_flag",
	"Resource Access Token Hook": "token",
}

// ParseWebhookEvent summarizes a VERIFIED GitLab delivery: the event type
// comes from X-Gitlab-Event, normalized onto the common WebhookEvent set;
// the action verb comes from the payload's object_attributes.action /
// object_attributes.state. Unrecognized or absent inputs yield "unknown" —
// never an error, since the delivery still triggers the level-triggered
// reconcile.
func (p *GitLabProvider) ParseWebhookEvent(_ context.Context, headers http.Header, payload []byte) (*WebhookEvent, error) {
	event := &WebhookEvent{Type: "unknown"}
	native := ""
	if headers != nil {
		native = headers.Get(gitlabEventHeader)
	}
	if mapped, ok := gitlabEventTypes[native]; ok {
		event.Type = mapped
	} else if native != "" {
		// Keep the provider's own name for log attribution when no
		// mapping exists; it is a string in a log line, not a branch
		// target anywhere.
		event.Type = native
	}
	event.Action = probeGitLabAction(payload)
	return event, nil
}

// probeGitLabAction pulls the action verb out of a GitLab payload
// (object_attributes.action, falling back to object_attributes.state for
// state-only hooks). Empty when absent.
func probeGitLabAction(payload []byte) string {
	type wire struct {
		ObjectAttributes struct {
			Action string `json:"action"`
			State  string `json:"state"`
		} `json:"object_attributes"`
	}
	var w wire
	if err := json.Unmarshal(payload, &w); err != nil {
		return ""
	}
	if w.ObjectAttributes.Action != "" {
		return w.ObjectAttributes.Action
	}
	return w.ObjectAttributes.State
}

// Snapshot is not implemented for GitLab yet — fail closed.
func (p *GitLabProvider) Snapshot(_ context.Context, _ string, _ SnapshotOptions) ([]NormalizedRecord, error) {
	return nil, gitlabNotImplemented("Snapshot")
}

// CreateComment is not implemented for GitLab yet — fail closed.
func (p *GitLabProvider) CreateComment(_ context.Context, _, _, _, _ string) (string, error) {
	return "", gitlabNotImplemented("CreateComment")
}

// UpdateIssue is not implemented for GitLab yet — fail closed (story 11.2
// outbound sync is GitHub-first; the seam method exists so the sync engine
// stays provider-neutral).
func (p *GitLabProvider) UpdateIssue(_ context.Context, _, _ string, _ IssueUpdate) error {
	return gitlabNotImplemented("UpdateIssue")
}

// CreateStatus is not implemented for GitLab yet — fail closed.
func (p *GitLabProvider) CreateStatus(_ context.Context, _, _ string, _ Status) error {
	return gitlabNotImplemented("CreateStatus")
}

// GetRepo is not implemented for GitLab yet — fail closed.
func (p *GitLabProvider) GetRepo(_ context.Context, _ string) (*Repository, error) {
	return nil, gitlabNotImplemented("GetRepo")
}
