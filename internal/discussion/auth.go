package discussion

import (
	"context"

	"github.com/google/uuid"
)

// Principal is the authenticated identity of the caller, as established by the
// upstream auth middleware (BFF / apiserver) and carried on the request
// context. Discussion write handlers stamp message provenance from this value
// — they never trust author fields supplied in the request body (cf. §7.3.1
// write-time auth+provenance, ISI-2224).
type Principal struct {
	ID   uuid.UUID
	Type AuthorType
	Name string
}

type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying the authenticated principal.
// Auth middleware calls this once the caller is authenticated; handlers read it
// back with PrincipalFromContext.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the authenticated principal on ctx. ok is false
// when no principal was set (unauthenticated request) or the principal has no
// identity — callers writing provenance MUST fail closed in that case.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	if !ok || p.ID == uuid.Nil {
		return Principal{}, false
	}
	return p, ok
}
