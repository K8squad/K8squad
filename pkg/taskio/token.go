// Package taskio implements the run-scoped agent task-io API seam (ISI-3601,
// Story S2 of the ISI-3588 agent-bootstrap study): the small, own-run-only
// surface a K8squad agent uses mid-run to re-read its task, post a comment,
// update its status, and check out (claim) its work item.
//
// Two halves live here:
//
//   - token.go — the run-scoped bearer token. It is a Paperclip-style,
//     run-lifetime-bounded credential bound to (RUN_ID, WORK_ITEM_ID,
//     principal). It reuses the platform's existing HS256 JWT machinery
//     (pkg/auth) — NO bespoke crypto — but mints through a DISTINCT issuer
//     ("ksquad-taskio") so a console session JWT can never be replayed as a
//     run token even though both may share one signing key (§AC5).
//   - handler.go — the get-task / post-comment / update-status / checkout HTTP
//     endpoints, each authorized from the token's own binding, never from
//     client-supplied path/params alone.
//
// Scope discipline (spike §4/§6): this is the agent's OWN-run seam only. It is
// deliberately NOT a general read-project / read-issue browser — that would
// re-introduce the pull-everything model the design rejected.
package taskio

import (
	"errors"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/auth"
)

// Issuer is the audience marker minted into the token's `iss` claim. It is what
// domain-separates run-scoped task-io tokens from console session JWTs under a
// shared signing key (§AC5): a token minted by the console session issuer
// ("ksquad-apiserver") fails Verify here, and vice versa.
const Issuer = "ksquad-taskio"

// DefaultTTL bounds a run token's lifetime when the caller passes ttl <= 0. The
// token is meant to live for the span of a Run; the mint site should pass the
// Run's own budget/deadline when it has one. Expiry is the run-lifetime bound
// AC5 requires — a token cannot outlive its usefulness window.
const DefaultTTL = time.Hour

// Env var names injected into the runtime container/subprocess (§AC6). These
// mirror Paperclip's PAPERCLIP_API_URL / PAPERCLIP_API_KEY / PAPERCLIP_TASK_ID
// injection. They join the existing curated KSQUAD_* set — never os.Environ, so
// no operator secret leaks into the task subprocess (the minimal-env invariant).
const (
	EnvCoordURL   = "KSQUAD_COORD_URL"   // in-cluster coord/apiserver base URL
	EnvCoordToken = "KSQUAD_COORD_TOKEN" // #nosec G101 -- this is the env var NAME the run token rides under, not a credential value
	EnvWorkItemID = "WORK_ITEM_ID"       // the Run's work item (also bound in the token)
	EnvRunID      = "RUN_ID"             // the Run uid (also bound in the token)
)

// ErrInvalidToken is returned when a token does not verify (bad signature,
// wrong issuer, expired, malformed, or missing its run binding). It wraps the
// underlying auth error so callers can errors.Is against either. No reason is
// surfaced to the remote caller — the handler maps this to 401.
var ErrInvalidToken = errors.New("taskio: invalid run token")

// ErrScopeMismatch is returned when a verified token is used against a run or
// work item other than the one it is bound to. The handler maps this to 403:
// the token is authentic but does not authorize the requested resource (§AC5).
var ErrScopeMismatch = errors.New("taskio: token not scoped to this run/work item")

// RunToken is the verified, run-scoped identity carried by a task-io call. Every
// endpoint derives RunID/WorkItemID from THIS (the token), never from
// client-supplied params alone, so a client cannot pivot to another run by
// changing a path segment (§AC5, technical-notes "Auth binding").
type RunToken struct {
	RunID      string
	WorkItemID string
	Principal  string // author_principal for comments / claim holder attribution
}

// Minter mints and verifies run-scoped tokens over the shared HS256 machinery.
// It is safe for concurrent use.
type Minter struct {
	iss *auth.JWTIssuer
}

// NewMinter builds a Minter from the shared HS256 signing key (the same
// KSQUAD_JWT_SIGNING_KEY the apiserver uses, so the operator can mint and the
// coord API can verify with one configured secret). key must be >= 32 bytes.
// ttl <= 0 defaults to DefaultTTL. The issuer is fixed to Issuer for audience
// separation.
func NewMinter(key []byte, ttl time.Duration) (*Minter, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	iss, err := auth.NewJWTIssuerWithIssuer(key, ttl, Issuer)
	if err != nil {
		return nil, fmt.Errorf("taskio: new minter: %w", err)
	}
	return &Minter{iss: iss}, nil
}

// TTL reports the configured token lifetime.
func (m *Minter) TTL() time.Duration { return m.iss.TTL() }

// Mint issues a token bound to (runID, workItemID, principal). runID and
// workItemID are required — a token with no run binding is refused, so a minted
// token always authorizes exactly one run's work item. principal may be empty
// (attribution falls back to the run) but is normally the agent name.
func (m *Minter) Mint(runID, workItemID, principal string) (string, error) {
	if runID == "" || workItemID == "" {
		return "", fmt.Errorf("taskio: mint requires runID and workItemID")
	}
	return m.iss.Mint(auth.Claims{
		Subject:    principal,
		RunID:      runID,
		WorkItemID: workItemID,
	})
}

// Verify checks the token's signature, issuer, and expiry and returns its run
// binding. A token that verifies cryptographically but carries no run/work-item
// binding is rejected (ErrInvalidToken) — it is not a run token. Any failure is
// opaque to the caller.
func (m *Minter) Verify(token string) (RunToken, error) {
	c, err := m.iss.Verify(token)
	if err != nil {
		return RunToken{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if c.RunID == "" || c.WorkItemID == "" {
		// Cryptographically valid but not a run-scoped token (e.g. a token that
		// somehow shares the issuer but lacks the binding) — fail closed.
		return RunToken{}, ErrInvalidToken
	}
	return RunToken{
		RunID:      c.RunID,
		WorkItemID: c.WorkItemID,
		Principal:  c.Subject,
	}, nil
}

// Authorize enforces own-run binding for a request that also names a run and/or
// work item in its path/params. Empty want* means "not client-supplied" and is
// not checked — the token's binding still governs. A non-empty want that
// disagrees with the token is ErrScopeMismatch (§AC5): a token for run A cannot
// touch run B even if the client points the path at B.
func (t RunToken) Authorize(wantRunID, wantWorkItemID string) error {
	if wantRunID != "" && wantRunID != t.RunID {
		return ErrScopeMismatch
	}
	if wantWorkItemID != "" && wantWorkItemID != t.WorkItemID {
		return ErrScopeMismatch
	}
	return nil
}
