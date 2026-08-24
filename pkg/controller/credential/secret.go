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

// Package credential implements the Story 7.7 zero-touch Claude OAuth
// lifecycle: a leader-elected credential controller (arch §5.2, §11.1,
// ADR-032) that watches per-user human-seat OAuth Secrets, refreshes the ~8h
// access token via the refresh token BEFORE it expires, and writes the new
// token back to the SAME per-user Secret — so many concurrent agent pods that
// merely MOUNT that Secret share one login and never handle token strings.
//
// The controller is registered on the controller-runtime Manager, whose
// LeaderElection is enabled (cmd/operator: --leader-elect): a Manager only
// runs its controllers on the elected leader, so there is exactly ONE active
// refresher across operator replicas. That single-owner guarantee is the whole
// point of story 7.7 — no per-pod refresh, no thundering-refresh race on the
// shared Secret (arch §5.2).
//
// Human-seat vs service-account enforcement (ADR-041) is structural, not a
// runtime check the reconciler must remember: the controller's Watch predicate
// matches ONLY Secrets labelled `ksquad.io/credential-class: human-seat`
// (LabelCredentialClass / ClassHumanSeat). A service-account credential (a
// long-lived API key, story 7.3, rotation = Secret update) is never selected,
// so the controller can never drive an OAuth refresh against a credential that
// has no refresh lifecycle. The human-seat OAuth lifecycle is opt-in by
// construction.
//
// This file defines the on-Secret data contract (which keys hold the token
// material) and the health surface (annotations screen 05 / story 8.6 reads to
// show connected / refreshing / expired) — everything a Secret carries so the
// controller, the shim's credinject mount, and the console agree without a
// side channel.
package credential

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// LabelCredentialClass is the human-seat vs service-account axis stamped on
	// a credential Secret. It mirrors the Agent's spec.credentialClass
	// (credinject.CredentialClass) so the enforcement axis is the SAME string
	// end to end. The controller's Watch predicate matches ONLY
	// ClassHumanSeat — the ADR-041 enforcement that a service-account
	// credential is never given the OAuth-refresh lifecycle.
	LabelCredentialClass = "ksquad.io/credential-class"

	// ClassHumanSeat is the LabelCredentialClass value the controller selects:
	// an interactive OAuth token bound to a human's subscription seat (Claude
	// Code OAuth, story 7.2). Matches credinject.ClassHumanSeat.
	ClassHumanSeat = "human-seat"

	// KeyAccessToken is the Secret data key holding the ~8h OAuth access token
	// the agent pods mount (as CLAUDE_CODE_OAUTH_TOKEN, via credinject's
	// default key "token" for a Claude-Code human seat). The controller
	// overwrites this key in place on every refresh — the mounted Secret is the
	// single source of truth, so concurrent pods pick up the new token on their
	// next kubelet Secret projection with no restart of the controller's doing.
	KeyAccessToken = "token"

	// KeyRefreshToken is the Secret data key holding the long-lived OAuth
	// refresh token. The controller reads it to mint a new access token and
	// writes back the (rotated) refresh token the provider returns. Agent pods
	// never read this key — only the controller does.
	KeyRefreshToken = "refreshToken"

	// KeyExpiresAt is the Secret data key holding the access token's expiry as
	// an RFC3339 timestamp. It is the clock the controller refreshes against:
	// there is no way to read an opaque token's expiry from its bytes, so the
	// provisioning flow (console "Connect Claude" / `ksquad auth login`, 7.7)
	// records it here and the controller keeps it current on every refresh.
	KeyExpiresAt = "expiresAt"

	// KeyConnectedAt is the Secret data key holding the RFC3339 timestamp of
	// the one-time login. It anchors the ~9-day refresh-window lapse: the
	// controller keeps refreshing indefinitely while the refresh token stays
	// valid; only when the provider rejects the refresh token
	// (ErrRefreshExpired) does the credential become expired and need re-login.
	KeyConnectedAt = "connectedAt"
)

const (
	// AnnotationState is the credential health the console (screen 05, story
	// 8.6) reads: connected / refreshing / expired / error. It is derived
	// state the controller owns — the user sees status but never token strings.
	AnnotationState = "ksquad.io/credential-state"

	// AnnotationExpiresAt echoes the access-token expiry onto an annotation so
	// the console can render "refreshes ~T" without decoding Secret data.
	AnnotationExpiresAt = "ksquad.io/credential-expires-at"

	// AnnotationLastRefresh records the RFC3339 time of the last successful
	// refresh — the health canary's heartbeat: a stale value with a near expiry
	// is the operator's early-warning that the refresher is wedged.
	AnnotationLastRefresh = "ksquad.io/credential-last-refresh"

	// AnnotationMessage carries a short human-readable reason for the current
	// state (e.g. "refresh token rejected — re-login required"). Never contains
	// token material.
	AnnotationMessage = "ksquad.io/credential-message"
)

// Credential health states (AnnotationState values). These are the exact
// strings story 8.6 surfaces on the console; keep them stable.
const (
	// StateConnected — a valid access token that is not yet inside the refresh
	// lead window. Nothing to do but wait.
	StateConnected = "connected"

	// StateRefreshing — the controller is minting a new access token from the
	// refresh token (transient; the console shows a spinner).
	StateRefreshing = "refreshing"

	// StateExpired — the refresh token itself was rejected (provider
	// invalid_grant) or the refresh window lapsed. The Run pauses
	// Paused(cred_expired) (story 7.4) and the console offers one-click
	// re-login (8.6). The controller does NOT auto-requeue an expired
	// credential — only a human re-login (a Secret update) revives it.
	StateExpired = "expired"

	// StateError — a misconfiguration that is not the user's OAuth window
	// lapsing: a human-seat Secret missing its refresh material, or an
	// unparseable expiry. Legible, fail-closed, and fixed by correcting the
	// Secret — never a silent no-op.
	StateError = "error"
)

// credState is the parsed, validated view of a human-seat OAuth Secret the
// reconciler operates on. Parsing once, up front, keeps the reconcile body a
// clean state machine over typed values rather than raw map lookups.
type credState struct {
	// refreshToken is the current OAuth refresh token (Secret KeyRefreshToken).
	refreshToken string
	// expiresAt is the access token's expiry (Secret KeyExpiresAt).
	expiresAt time.Time
	// connectedAt is the one-time-login timestamp (Secret KeyConnectedAt); zero
	// if the provisioning flow did not record it (older Secrets).
	connectedAt time.Time
}

// parseCredential extracts and validates the OAuth material from a human-seat
// Secret. It fails CLOSED with a descriptive error (surfaced as StateError)
// when the required refresh material or a parseable expiry is missing — a
// human-seat Secret the controller cannot refresh is a configuration bug the
// operator must see, never a credential the controller silently skips.
func parseCredential(s *corev1.Secret) (credState, error) {
	rt, ok := s.Data[KeyRefreshToken]
	if !ok || len(rt) == 0 {
		return credState{}, fmt.Errorf("human-seat credential Secret %q/%q has no %q key; cannot refresh (re-run the one-time login)", s.Namespace, s.Name, KeyRefreshToken)
	}

	rawExpiry, ok := s.Data[KeyExpiresAt]
	if !ok || len(rawExpiry) == 0 {
		return credState{}, fmt.Errorf("human-seat credential Secret %q/%q has no %q key; cannot know when to refresh", s.Namespace, s.Name, KeyExpiresAt)
	}
	expiresAt, err := time.Parse(time.RFC3339, string(rawExpiry))
	if err != nil {
		return credState{}, fmt.Errorf("human-seat credential Secret %q/%q has unparseable %q %q: %w", s.Namespace, s.Name, KeyExpiresAt, string(rawExpiry), err)
	}

	cs := credState{refreshToken: string(rt), expiresAt: expiresAt}
	// connectedAt is best-effort: absence or an unparseable value is not fatal
	// (the refresh-token rejection path, not the timestamp, is the real expiry
	// signal), so a bad value degrades to zero rather than blocking refresh.
	if raw, ok := s.Data[KeyConnectedAt]; ok && len(raw) > 0 {
		if t, err := time.Parse(time.RFC3339, string(raw)); err == nil {
			cs.connectedAt = t
		}
	}
	return cs, nil
}
