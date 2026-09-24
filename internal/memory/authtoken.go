package memory

import (
	"strings"

	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
)

// authtoken.go — the concrete AuthoringTokenVerifier over the run capability
// token (ADR-0024a S3/D2, ISI-4869). It adapts pkg/mcpauthtoken.Minter (the
// shared HS256 mint/verify machinery, distinct issuer "ksquad-mcp-authoring")
// into the memory edge's AuthoringTokenVerifier seam so toolmcp.go can build a
// server-authenticated AgentSession from a verified token without importing the
// token package directly. Kept beside the resolver/gate it feeds.

// mcpAuthTokenVerifier verifies a bearer run capability token and maps its
// verified claims into an AgentSession (ViaToken set). Identity, tenancy and
// capabilities come only from the token — never a client X-* header.
type mcpAuthTokenVerifier struct {
	m *mcpauthtoken.Minter
}

// NewMCPAuthTokenVerifier builds the verifier from a run-capability-token minter,
// or returns nil (token path off) when the minter is nil — the caller then leaves
// the token-auth mode unwired and the /mcp edge serves only the header (BFF)
// path. Wired in cmd/memory/main.go from the distributed HS256 signing key
// (D2 option (a)); no key ⇒ nil minter ⇒ inert.
func NewMCPAuthTokenVerifier(m *mcpauthtoken.Minter) AuthoringTokenVerifier {
	if m == nil {
		return nil
	}
	return &mcpAuthTokenVerifier{m: m}
}

// Verify implements AuthoringTokenVerifier: verify the token and lift its
// verified claims into the server-authenticated session. A verification failure
// is returned verbatim (the edge maps it to a 401); it never yields a partial or
// header-derived session.
func (v *mcpAuthTokenVerifier) Verify(bearer string) (AgentSession, error) {
	c, err := v.m.Verify(bearer)
	if err != nil {
		return AgentSession{}, err
	}
	return AgentSession{
		TeamID:       c.TeamID,
		Principal:    c.Principal,
		AgentID:      c.AgentID,
		RunID:        c.RunID,
		Capabilities: c.Capabilities,
		ViaToken:     true,
	}, nil
}

// bearerToken extracts the credential from an "Authorization: Bearer <token>"
// header value, case-insensitively on the scheme. It returns "" when the header
// is empty or not a bearer scheme, so an absent/foreign Authorization header
// simply routes to the header (BFF) path.
func bearerToken(h string) string {
	const prefix = "Bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
