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

// Package mcpauthtoken is the run capability token for the ADR-0024a dispatched
// agent authoring lane (D2, ISI-4869 / epic ISI-4855). It is the SANDBOX-side
// credential that lets a dispatched agent reach the internal ksquad-memory
// `/mcp` authoring surface (work_item_create / _update / _assign) with the
// capabilities its role was granted (S4) — and NOTHING more.
//
// It reuses the platform's existing HS256 JWT machinery (pkg/auth) — NO bespoke
// crypto — but mints through a DISTINCT issuer ("ksquad-mcp-authoring") so a
// token minted for another audience (a console session JWT under
// "ksquad-apiserver", or a run task-io token under "ksquad-taskio") can never be
// replayed against the authoring edge, even under a shared signing key. This is
// the same audience-separation-over-one-key pattern pkg/taskio established
// (ISI-3601 §AC5).
//
// The security contract (ADR-0024a D2): the capabilities are baked into the
// token AT MINT TIME from the agent's grant (S4), by the control plane, so the
// sandbox cannot widen them. The memory `/mcp` edge derives the caller's team,
// principal, agent, run and capabilities from the VERIFIED claims and discards
// any client-supplied X-* identity headers on the token path — identity is
// stamped by the control plane at mint and verified by the edge, never asserted
// by the untrusted sandbox.
//
// Cross-process key note (D2, Henrik's readiness call): pkg/auth's model is an
// in-process HS256 key that never leaves the apiserver (ADR-033). Here the
// control plane (operator/apiserver) mints and the SEPARATE cmd/memory process
// verifies, so the signing key must reach both. The first cut uses a shared
// HS256 secret delivered to both Deployments via Helm; the verifier could then
// also mint, which is acceptable strictly inside the control-plane trust
// boundary. Moving to an asymmetric alg (so memory holds only a public key)
// would require extending pkg/auth (HS256-only today) and is the documented
// alternative.
package mcpauthtoken

import (
	"errors"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/auth"
)

// Issuer is the audience marker minted into the token's `iss` claim. It is what
// domain-separates run-scoped authoring tokens from console session JWTs
// ("ksquad-apiserver") and run task-io tokens ("ksquad-taskio") under a shared
// signing key: a token minted by another issuer fails Verify here, and vice
// versa (ADR-0024a D2, mirroring pkg/taskio.Issuer).
const Issuer = "ksquad-mcp-authoring"

// DefaultTTL bounds an authoring token's lifetime when the caller passes
// ttl <= 0. The token is meant to live for the span of a Run; the mint site
// should pass the Run's own budget/deadline when it has one.
const DefaultTTL = time.Hour

// ErrInvalidToken is returned when a token does not verify (bad signature,
// wrong issuer, expired, malformed, or missing its authoring binding). It wraps
// the underlying auth error so callers can errors.Is against either. No reason
// is surfaced to the remote caller — the memory edge maps this to a denial.
var ErrInvalidToken = errors.New("mcpauthtoken: invalid run capability token")

// Claims is the verified, control-plane-stamped identity an authoring MCP call
// runs under. Every field is derived from the token — never a client-supplied
// header or tool argument — so a sandbox cannot forge authorship or widen its
// capabilities. It mirrors the memory edge's AgentSession shape (team /
// principal / agent / run / capabilities) so the token path can build the
// session directly from the verified claim set.
type Claims struct {
	// TeamID is the caller's tenancy scope (the authoring team). Required.
	TeamID string
	// Principal is the §6.5 audit author (Run.Spec.OwnedBy). May be empty.
	Principal string
	// AgentID is the dispatched agent whose grant seeded Capabilities. Required —
	// it is matched against the work item's dispatch claim by S5 (custody-match).
	AgentID string
	// RunID is the authoring Run. Required.
	RunID string
	// Capabilities is the grant baked in at mint time from the agent's role
	// grant (S4), e.g. ["work_item.author"]. The edge's capability gate reads
	// ONLY this, never a client X-Agent-Capabilities header, on the token path.
	Capabilities []string
}

// Minter mints and verifies run capability tokens over the shared HS256
// machinery. It is safe for concurrent use.
type Minter struct {
	iss *auth.JWTIssuer
}

// NewMinter builds a Minter from the shared HS256 signing key (the same
// KSQUAD_JWT_SIGNING_KEY the apiserver uses — so the control plane can mint and
// cmd/memory can verify with one configured secret, D2 option (a)). key must be
// >= 32 bytes. ttl <= 0 defaults to DefaultTTL. The issuer is fixed to Issuer
// for audience separation.
func NewMinter(key []byte, ttl time.Duration) (*Minter, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	iss, err := auth.NewJWTIssuerWithIssuer(key, ttl, Issuer)
	if err != nil {
		return nil, fmt.Errorf("mcpauthtoken: new minter: %w", err)
	}
	return &Minter{iss: iss}, nil
}

// TTL reports the configured token lifetime.
func (m *Minter) TTL() time.Duration { return m.iss.TTL() }

// Mint issues a token bound to the authoring claims. teamID, agentID and runID
// are required — a token without a fully-formed authoring binding is refused, so
// a minted token always authorizes exactly one run's authoring under one agent's
// granted capabilities. capabilities is baked in from the agent's grant (S4) at
// mint time; the sandbox that receives the token cannot widen it (a wider set
// would need a fresh mint from the control plane).
func (m *Minter) Mint(c Claims) (string, error) {
	if c.TeamID == "" || c.AgentID == "" || c.RunID == "" {
		return "", fmt.Errorf("mcpauthtoken: mint requires teamID, agentID and runID")
	}
	return m.iss.Mint(auth.Claims{
		Subject: c.Principal,
		TeamID:  c.TeamID,
		AgentID: c.AgentID,
		RunID:   c.RunID,
		Scopes:  c.Capabilities,
	})
}

// Verify checks the token's signature, issuer and expiry and returns its
// authoring binding. A token that verifies cryptographically but carries no
// team/agent/run binding is rejected (ErrInvalidToken) — it is not an authoring
// token (e.g. a task-io token that somehow shared the issuer but lacks the
// binding). Any failure is opaque to the caller.
func (m *Minter) Verify(token string) (Claims, error) {
	c, err := m.iss.Verify(token)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if c.TeamID == "" || c.AgentID == "" || c.RunID == "" {
		return Claims{}, ErrInvalidToken
	}
	return Claims{
		TeamID:       c.TeamID,
		Principal:    c.Subject,
		AgentID:      c.AgentID,
		RunID:        c.RunID,
		Capabilities: c.Scopes,
	}, nil
}
