package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ============================================================================
// Epic 15 HTTP surface (15.1 + 15.2, ISI-2920): /auth/login|refresh|logout|me and
// the admin-scoped /admin/users CRUD, mounted on the apiserver root router.
//
//   - /auth/login is the ONLY unauthenticated route (plus probes): it is where
//     credentials enter; every other auth route resolves the opaque ksquad_session
//     cookie itself through pkg/auth (same fail-closed predicate as the §13 choke
//     point — these routes are the cookie's ISSUER, so they cannot ride it).
//   - /admin/users sits behind requireAdmin: session-authenticated AND
//     global_role=admin, else 401/403 (deny-by-default, spec 15.2).
//   - Passwords never appear in responses or logs; user JSON never carries the
//     argon2id hash (auth.User.PasswordHash is json:"-").
// ============================================================================

// AuthRoutesOptions wires the auth surface onto the host.
type AuthRoutesOptions struct {
	// Service is the pkg/auth core. Nil ⇒ routes are not mounted (host keeps the
	// pre-Epic-15 shape: every gated route stays 401 via the deny-all resolver).
	Service *auth.Service
	// Authenticator resolves the session cookie for the admin gate (the same
	// CookieAuthenticator the §13 choke point uses).
	Authenticator discussion.Authenticator
	// CookieName overrides the session cookie name (default SessionCookieName; must
	// match the BFF's KSQUAD_SESSION_COOKIE).
	CookieName string
	// SecureCookies sets the Secure attribute on session cookies (chart TLS, 9.5;
	// false only for a local http dev run).
	SecureCookies bool
	// TrustedProxies is the CIDR set whose X-Forwarded-For the login limiter may
	// trust (empty ⇒ XFF ignored, socket address used — PR #90 review finding 1).
	TrustedProxies []*net.IPNet
	// AllowedOrigins lists browser origins accepted on cookie-authenticated state
	// changes (empty ⇒ strict same-host Origin matching — PR #90 review finding 6).
	AllowedOrigins []string
	// Audit appends an admin-mutation event to the append-only coord.audit_log
	// (§6.5, ADR-040). Nil ⇒ audit events are logged only.
	Audit func(ctx context.Context, eventType, principal string, payload map[string]any)
}

// mountAuthRoutes installs /auth/* and /admin/users on the root router.
func (s *Server) mountAuthRoutes(opts AuthRoutesOptions) {
	svc := opts.Service
	if svc == nil {
		return
	}
	name := opts.CookieName
	if name == "" {
		name = SessionCookieName
	}
	cookies := cookieConfig{secure: opts.SecureCookies, name: name, maxAge: int(svc.SessionTTL().Seconds())}

	login := s.router.Path("/auth/login").Subrouter()
	login.Use(maxBytesBody(4096))
	login.HandleFunc("", s.authLogin(svc, cookies, opts.TrustedProxies)).Methods(http.MethodPost)

	refresh := s.router.Path("/auth/refresh").Subrouter()
	refresh.Use(maxBytesBody(4096))
	refresh.Use(sameOriginGuard(opts.AllowedOrigins))
	refresh.HandleFunc("", s.authRefresh(svc, cookies)).Methods(http.MethodPost)

	logout := s.router.Path("/auth/logout").Subrouter()
	logout.Use(sameOriginGuard(opts.AllowedOrigins))
	logout.HandleFunc("", s.authLogout(svc, cookies)).Methods(http.MethodPost)

	me := s.router.Path("/auth/me").Subrouter()
	me.HandleFunc("", s.authMe(svc, cookies)).Methods(http.MethodGet)

	admin := s.router.PathPrefix("/admin/users").Subrouter()
	admin.Use(requireAdmin(opts.Authenticator))
	admin.Use(sameOriginGuard(opts.AllowedOrigins))
	admin.Use(maxBytesBody(8192))
	admin.HandleFunc("", s.adminCreateUser(svc, opts)).Methods(http.MethodPost)
	admin.HandleFunc("", s.adminListUsers(svc)).Methods(http.MethodGet)
	admin.HandleFunc("/{id}", s.adminGetUser(svc)).Methods(http.MethodGet)
	admin.HandleFunc("/{id}", s.adminPatchUser(svc, opts)).Methods(http.MethodPatch)
	admin.HandleFunc("/{id}", s.adminDeleteUser(svc, opts)).Methods(http.MethodDelete)
}

type cookieConfig struct {
	secure bool
	name   string
	maxAge int
}

// setSessionCookie writes the opaque HttpOnly session cookie (§12.3 / ADR-033).
func setSessionCookie(w http.ResponseWriter, c cookieConfig, token string) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure is config-driven (chart TLS by default; false only for a documented local http dev run); HttpOnly and SameSite=Lax are always set.
		Name:     c.name,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   c.maxAge,
	})
}

// clearSessionCookie expires the cookie client-side (server row is already revoked).
func clearSessionCookie(w http.ResponseWriter, c cookieConfig) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- same config-driven Secure flag as setSessionCookie; HttpOnly and SameSite=Lax are always set.
		Name: c.name, Value: "", Path: "/", HttpOnly: true, Secure: c.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// maxBytesBody bounds request bodies on credential-bearing routes (defense in depth).
func maxBytesBody(n int64) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// sameOriginGuard is the CSRF defense for cookie-authenticated state changes
// (PR #90 review finding 6; SameSite=Lax alone is not sufficient — top-level
// cross-site POSTs ride Lax). Browsers ALWAYS send Origin (and Sec-Fetch-Site)
// on cross-site state changes; a mismatched Origin or a cross-site fetch-site
// is rejected. Absent headers (curl, the BFF's server-side proxy hop) pass —
// non-browser clients are not CSRF subjects.
func sameOriginGuard(allowed []string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, r.Host, allowed) {
					writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
					return
				}
				if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" &&
					sfs != "same-origin" && sfs != "same-site" && sfs != "none" {
					writeJSONError(w, http.StatusForbidden, "cross-site request rejected")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originAllowed checks one Origin header value against the request Host and the
// configured allowlist.
func originAllowed(origin, host string, allowed []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(a), origin) ||
			strings.EqualFold(strings.TrimSpace(a), u.Host) {
			return true
		}
	}
	return false
}

// adminContextKey carries the acting admin's AuthorContext through the admin routes.
type adminContextKey struct{}

// requireAdmin is the 15.2 admin gate: session-authenticated + global_role=admin.
// The resolved AuthorContext rides the request for provenance (team defaulting,
// audit actor) — handlers never re-read headers for identity.
func requireAdmin(authn discussion.Authenticator) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authn == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "admin surface unavailable")
				return
			}
			author, ok := authn.Authenticate(r)
			if !ok {
				writeJSONError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if !author.IsAdmin {
				writeJSONError(w, http.StatusForbidden, "admin role required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminContextKey{}, author)))
		})
	}
}

// actingAdmin returns the requireAdmin-resolved AuthorContext (zero value if absent).
func actingAdmin(r *http.Request) discussion.AuthorContext {
	author, _ := r.Context().Value(adminContextKey{}).(discussion.AuthorContext)
	return author
}

// ── /auth/* handlers ──────────────────────────────────────────────────────────

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// NOTE: no group/claim input here, ever (PR #90 review finding 3): groups are
	// IdP-asserted claims consumed inside a trusted token exchange (15.9), not
	// request-body data a client can self-assert.
}

func (s *Server) authLogin(svc *auth.Service, cookies cookieConfig, trusted []*net.IPNet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return // decodeJSON answered
		}
		if req.Username == "" || req.Password == "" {
			writeJSONError(w, http.StatusBadRequest, "username and password are required")
			return
		}
		ip := auth.ClientIP(trusted, r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
		res, err := svc.Login(r.Context(), req.Username, req.Password, ip)
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			writeJSONError(w, http.StatusTooManyRequests, "too many attempts")
			return
		case errors.Is(err, auth.ErrInvalidCredentials):
			// One opaque 401: unknown user / wrong password / deactivated are indistinguishable.
			writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
			return
		case err != nil:
			log.Printf("apiserver: auth login: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "login failed")
			return
		}
		setSessionCookie(w, cookies, res.SessionToken)
		writeJSON(w, http.StatusOK, loginResponse{
			AccessToken: res.AccessToken,
			TokenType:   "Bearer",
			ExpiresIn:   res.ExpiresIn,
			User:        publicUser(res.User),
		})
	}
}

type loginResponse struct {
	AccessToken string      `json:"accessToken"`
	TokenType   string      `json:"tokenType"`
	ExpiresIn   int64       `json:"expiresIn"`
	User        publicUserV `json:"user"`
}

type publicUserV struct {
	ID            uuid.UUID  `json:"id"`
	Username      string     `json:"username"`
	Principal     string     `json:"principal"`
	Email         *string    `json:"email,omitempty"`
	TeamID        uuid.UUID  `json:"teamId"`
	GlobalRole    string     `json:"globalRole"`
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     *string    `json:"createdBy,omitempty"`
	DeactivatedAt *time.Time `json:"deactivatedAt,omitempty"`
}

// publicUser maps the stored user to its API shape (never the password hash).
func publicUser(u *auth.User) publicUserV {
	return publicUserV{
		ID: u.ID, Username: u.Username, Principal: u.Principal, Email: u.Email,
		TeamID: u.TeamID, GlobalRole: u.GlobalRole, CreatedAt: u.CreatedAt,
		CreatedBy: u.CreatedBy, DeactivatedAt: u.DeactivatedAt,
	}
}

func (s *Server) authRefresh(svc *auth.Service, cookies cookieConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := sessionTokenFromRequest(r, cookies.name)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "no valid session")
			return
		}
		res, err := svc.Refresh(r.Context(), token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "no valid session")
			return
		}
		setSessionCookie(w, cookies, res.SessionToken)
		writeJSON(w, http.StatusOK, loginResponse{
			AccessToken: res.AccessToken,
			TokenType:   "Bearer",
			ExpiresIn:   res.ExpiresIn,
			User:        publicUser(res.User),
		})
	}
}

func (s *Server) authLogout(svc *auth.Service, cookies cookieConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := sessionTokenFromRequest(r, cookies.name); ok {
			if err := svc.Logout(r.Context(), token); err != nil {
				log.Printf("apiserver: auth logout: %v", err)
			}
		}
		clearSessionCookie(w, cookies)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) authMe(svc *auth.Service, cookies cookieConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := sessionTokenFromRequest(r, cookies.name)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "no valid session")
			return
		}
		u, err := svc.Me(r.Context(), token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "no valid session")
			return
		}
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}

// sessionTokenFromRequest pulls the opaque cookie value ("" ⇒ not ok).
func sessionTokenFromRequest(r *http.Request, name string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// ── /admin/users handlers (15.2) ──────────────────────────────────────────────

type createUserRequest struct {
	Username   string  `json:"username"`
	Password   string  `json:"password"`
	GlobalRole string  `json:"globalRole,omitempty"`
	Email      *string `json:"email,omitempty"`
	TeamID     string  `json:"teamId,omitempty"`
}

func (s *Server) adminCreateUser(svc *auth.Service, opts AuthRoutesOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createUserRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		if req.Username == "" || len(req.Password) < 8 {
			writeJSONError(w, http.StatusBadRequest, "username and a password of at least 8 characters are required")
			return
		}
		if req.GlobalRole == "" {
			req.GlobalRole = auth.RoleUser
		}
		if req.GlobalRole != auth.RoleAdmin && req.GlobalRole != auth.RoleUser {
			writeJSONError(w, http.StatusBadRequest, "globalRole must be admin or user")
			return
		}

		// Tenancy stays un-skippable: a new user inherits the creating admin's Team
		// unless the admin assigns one explicitly (0006 v1 1:1 model).
		teamID := actingAdmin(r).TeamID
		if req.TeamID != "" {
			parsed, err := uuid.Parse(req.TeamID)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "teamId must be a uuid")
				return
			}
			teamID = parsed
		}

		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			log.Printf("apiserver: hash password: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "create user failed")
			return
		}

		createdBy := actingAdmin(r).Principal
		u := &auth.User{
			Username: req.Username, PasswordHash: hash, TeamID: teamID,
			GlobalRole: req.GlobalRole, Email: req.Email, CreatedBy: &createdBy,
		}
		if err := svc.Users.Create(r.Context(), u); err != nil {
			if strings.Contains(err.Error(), "duplicate key") {
				writeJSONError(w, http.StatusConflict, "username or email already exists")
				return
			}
			log.Printf("apiserver: create user: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "create user failed")
			return
		}
		auditAdminMutation(r.Context(), opts, "user_created", actingAdmin(r).Principal, map[string]any{
			"userId": u.ID.String(), "username": u.Username, "globalRole": u.GlobalRole,
		})
		writeJSON(w, http.StatusCreated, publicUser(u))
	}
}

func (s *Server) adminListUsers(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := pagination(r)
		users, total, err := svc.Users.List(r.Context(), limit, offset)
		if err != nil {
			log.Printf("apiserver: list users: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "list users failed")
			return
		}
		items := make([]publicUserV, 0, len(users))
		for _, u := range users {
			items = append(items, publicUser(u))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "limit": limit, "offset": offset})
	}
}

func (s *Server) adminGetUser(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		u, err := svc.Users.ByID(r.Context(), id)
		if errors.Is(err, auth.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such user")
			return
		}
		if err != nil {
			log.Printf("apiserver: get user: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "get user failed")
			return
		}
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}

type patchUserRequest struct {
	GlobalRole *string `json:"globalRole,omitempty"`
	Email      *string `json:"email,omitempty"`
}

func (s *Server) adminPatchUser(svc *auth.Service, opts AuthRoutesOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		var req patchUserRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		if req.GlobalRole == nil && req.Email == nil {
			writeJSONError(w, http.StatusBadRequest, "nothing to update")
			return
		}
		if req.GlobalRole != nil && *req.GlobalRole != auth.RoleAdmin && *req.GlobalRole != auth.RoleUser {
			writeJSONError(w, http.StatusBadRequest, "globalRole must be admin or user")
			return
		}
		u, err := svc.Users.Update(r.Context(), id, auth.UserUpdate{GlobalRole: req.GlobalRole, Email: req.Email})
		if errors.Is(err, auth.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such user")
			return
		}
		if errors.Is(err, auth.ErrLastAdmin) {
			writeJSONError(w, http.StatusConflict, "cannot demote the last active admin")
			return
		}
		if err != nil {
			log.Printf("apiserver: patch user: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "update user failed")
			return
		}
		author := actingAdmin(r)
		auditAdminMutation(r.Context(), opts, "user_updated", author.Principal, map[string]any{
			"userId": u.ID.String(), "username": u.Username, "globalRole": u.GlobalRole,
		})
		writeJSON(w, http.StatusOK, publicUser(u))
	}
}

func (s *Server) adminDeleteUser(svc *auth.Service, opts AuthRoutesOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		u, err := svc.Users.ByID(r.Context(), id)
		if errors.Is(err, auth.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such user")
			return
		}
		if err != nil {
			log.Printf("apiserver: get user: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "delete user failed")
			return
		}
		if err := svc.Users.Deactivate(r.Context(), id); err != nil {
			if errors.Is(err, auth.ErrLastAdmin) {
				writeJSONError(w, http.StatusConflict, "cannot deactivate the last active admin")
				return
			}
			if errors.Is(err, auth.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "no such user")
				return
			}
			log.Printf("apiserver: deactivate user: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "delete user failed")
			return
		}
		// Every live session dies with the account (15.2 DELETE semantics). The
		// resolver's deactivated_at filter already fails them closed even if this
		// revocation were lost.
		if err := svc.Sessions.RevokeAllForUser(r.Context(), id); err != nil {
			log.Printf("apiserver: revoke sessions for deactivated user: %v", err)
		}
		author := actingAdmin(r)
		auditAdminMutation(r.Context(), opts, "user_deactivated", author.Principal, map[string]any{
			"userId": u.ID.String(), "username": u.Username,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// auditAdminMutation emits the §6.5 append-only audit event (or logs when no
// audit sink is wired). Best-effort by design: the mutation has applied; a failed
// audit append is a loud warning, never a silent drop. The context is DETACHED
// from the request (PR #90 review: a client disconnect right after the mutation
// must not cancel the audit write that records it).
func auditAdminMutation(ctx context.Context, opts AuthRoutesOptions, eventType, principal string, payload map[string]any) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if opts.Audit != nil {
		opts.Audit(detached, eventType, principal, payload)
		return
	}
	log.Printf("apiserver: audit %s by %s: %v", eventType, principal, payload)
}

// decodeJSON parses a JSON body, answering the error itself. Unknown fields are
// tolerated (forward-compatible clients); malformed JSON is a 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return err
	}
	return nil
}

func userIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(mux.Vars(r)["id"])
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "user id must be a uuid")
		return uuid.UUID{}, false
	}
	return id, true
}

func intQuery(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func pagination(r *http.Request) (limit, offset int) {
	limit = intQuery(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset = intQuery(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
