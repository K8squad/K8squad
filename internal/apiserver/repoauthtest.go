package apiserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Repo auth Test-connection (E4-S1 / ISI-3683, AD-7, FR-5) — POST
// /api/projects/repo-auth/test gives the connect-repo wizard (E4-S2) a real
// green/red on a STORED PAT without ever handing the secret back.
// ============================================================================
//
// The contract (mirrors the E3-S2 model Test-connection shape):
//
//  1. Server-side probe only. The caller sends the repo URL plus a
//     credentialSecretRef NAME — never the token. The apiserver resolves the
//     Secret from the caller's team namespace (§12.1 tenancy root; a
//     cross-tenant name is structurally a 404) and probes the provider
//     server-side: GitHub GET /user (whoami — proves the credential
//     authenticates) plus the repository visibility the /user payload already
//     carries (repo read scope), exactly the GET /user + repo-count probe AD-7
//     specifies.
//
//  2. {ok, detail} only. The response never carries the token or the Secret
//     bytes (NFR-2): detail names the login and a count — information the
//     token owner already has. Every error branch names the failing STEP,
//     never the credential material.
//
//  3. Cached Team annotation (AD-2). The last result is stamped on the Team
//     CR as ksquad.io/onboarding-test-connection-repo = passed|failed — the
//     same annotation family the onboarding read model (E1-S1) already
//     projects, so the Launchpad "project" milestone can un-complete on a
//     recorded failure without a second read. The annotation write is
//     best-effort: a failure logs and moves on, the probe answer is already
//     sent.
//
//  4. Honest provider floor. v1 probes github.com only (the RepoSyncSpec
//     provider floor); any other host answers 422 naming the v1 floor rather
//     than a fabricated GitHub-Enterprise guess.
//
// RBAC NOTE: reading the Secret needs secrets:get in the team namespace — a
// grant the apiserver SA does NOT hold today (ISI-3546/ISI-3671 deliberately
// gave it secrets:create only). A Forbidden read is surfaced as 502 with a
// loud log naming the missing grant; the follow-up DevOps+Security issue
// (shared with E3-S2, which needs the identical read) owns the scoped grant.

// repoProbeTimeout bounds one server-side probe; the wizard's Test button must
// not hang on a black-holed egress path.
const repoProbeTimeout = 10 * time.Second

// repoAuthTokenSecretKey is the Secret data key this surface reads when the
// request's credentialSecretRef leaves Key empty — the SAME key contract the
// repo-sync reconciler reads (pkg/controller/reposync tokenSecretKey), so a
// Secret the wizard tested is the Secret the reconciler will later read
// (write/read key agreement, the E3-S1 F1 lesson).
const repoAuthTokenSecretKey = "token"

// RepoAuthTestClient is the cluster seam the repo test path needs: Secret Get
// (the stored PAT) plus Team List/Update (UID→namespace resolution and the
// cached annotation, same discipline as secretwrite.go). Production wires a
// direct (uncached) controller-runtime client — a probe must read the Secret
// as it exists at the API server, not a cache snapshot. Tests wire a fake.
type RepoAuthTestClient interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
}

// RepoProber is the provider seam: probe one credential against a repo URL,
// returning the authenticated login and the visible repository count. GitHub
// is the v1 implementation; keeping it an interface means the handler tests
// never need the network, and a GitLab/Gitea probe later is a new
// implementation, not a handler change (the pkg/scm seam discipline).
type RepoProber interface {
	Probe(ctx context.Context, repoURL, token string) (login string, repoCount int64, err error)
}

// GitHubRepoProber probes GitHub with the PAT: GET /user (whoami) + the
// repository visibility the /user payload already carries (public_repos +
// total_private_repos) — one request, no repo listing. A 401/403 from GitHub
// is an auth failure, not a transport failure: it maps to ok=false with the
// provider's own status, never a 5xx.
type GitHubRepoProber struct{}

// Probe implements RepoProber for github.com URLs.
func (GitHubRepoProber) Probe(ctx context.Context, repoURL, token string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, repoProbeTimeout)
	defer cancel()
	if token == "" {
		return "", 0, errors.New("empty provider credential")
	}
	gh := github.NewClient(nil).WithAuthToken(token)
	user, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return "", 0, err
	}
	// total_private_repos is only populated when the token carries repo read
	// scope; absent just means the count reflects public visibility only.
	repos := int64(user.GetPublicRepos()) + user.GetTotalPrivateRepos()
	return user.GetLogin(), repos, nil
}

// repoAuthTestRequest is the POST /api/projects/repo-auth/test wire body: the
// repo the wizard is connecting plus the STORED credential that will back it.
// camelCase credentialSecretRef reuses the compose secretRefWire shape, so the
// console round-trips one ref form through the compose apply and this test.
type repoAuthTestRequest struct {
	URL                 string        `json:"url"`
	CredentialSecretRef secretRefWire `json:"credentialSecretRef"`
}

// repoAuthTestResult is the response: {ok, detail} only (AD-7). detail is
// human-readable and safe to render verbatim in the console (the E5 verbatim
// error discipline): it names the login/count on success and the failing step
// on failure — never the credential material.
type repoAuthTestResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// RepoAuthTestService is the E4-S1 test model: one probe per request,
// team-scoped secret resolution, {ok,detail} answer, cached annotation.
type RepoAuthTestService struct {
	client RepoAuthTestClient
	prober RepoProber
}

// NewRepoAuthTestService builds the repo test model. The client's scheme MUST
// have corev1 and ksquadv1 registered (NewRepoAuthTestClient guarantees both).
func NewRepoAuthTestService(c RepoAuthTestClient, p RepoProber) *RepoAuthTestService {
	return &RepoAuthTestService{client: c, prober: p}
}

// validateRepoAuthTest is the fail-closed field validation. Errors name the
// field and the rule — never the ref's target, never any material.
func validateRepoAuthTest(req repoAuthTestRequest) []fieldError {
	var errs []fieldError
	if req.URL == "" {
		errs = append(errs, fieldError{Field: "url", Message: "is required"})
	} else if !supportedRepoHost(req.URL) {
		errs = append(errs, fieldError{Field: "url", Message: "v1 supports github.com repositories only"})
	}
	if msg := dns1123Name(req.CredentialSecretRef.Name); msg != "" {
		errs = append(errs, fieldError{Field: "credentialSecretRef.name", Message: msg})
	}
	return errs
}

// supportedRepoHost reports whether the URL is an http(s) github.com repo URL
// — the v1 provider floor (RepoSyncSpec.Provider). Host comparison is
// case-insensitive; any userinfo is ignored by url.Parse hostname extraction,
// so a token smuggled in the URL is not treated as a host.
func supportedRepoHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || host == "www.github.com"
}

// handleRepoAuthTest is the handler behind POST /api/projects/repo-auth/test.
func (s *RepoAuthTestService) handleRepoAuthTest(w http.ResponseWriter, r *http.Request) {
	author, ok := discussion.AuthFromContext(r.Context())
	if !ok || author.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	var req repoAuthTestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	if errs := validateRepoAuthTest(req); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation failed",
			"fields": errs,
		})
		return
	}

	team, ns, err := s.resolveTeam(r.Context(), author.TeamID.String())
	if errors.Is(err, ErrTeamNamespaceUnresolved) {
		writeJSONError(w, http.StatusNotFound, "no team namespace for this caller")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "team scope resolution unavailable")
		return
	}

	// The token is read into a local, used for the probe, and never crosses a
	// log line or a response field (NFR-2). The default data key is the SAME
	// contract repo-sync reads, so the probe sees the credential exactly as
	// the reconciler later will.
	key := req.CredentialSecretRef.Key
	if key == "" {
		key = repoAuthTokenSecretKey
	}
	secret := &corev1.Secret{}
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: req.CredentialSecretRef.Name}, secret); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeJSONError(w, http.StatusNotFound, "credential not found in this team")
		case apierrors.IsForbidden(err):
			log.Printf("apiserver: repo-auth test read FORBIDDEN for %s/%s principal=%s: the apiserver SA lacks the scoped secrets:get grant (E3-S2/E4-S1 follow-up)", ns, req.CredentialSecretRef.Name, author.Principal)
			writeJSONError(w, http.StatusBadGateway, "credential read grant is not configured on this cluster")
		default:
			log.Printf("apiserver: repo-auth test read failed for %s/%s: %v", ns, req.CredentialSecretRef.Name, err)
			writeJSONError(w, http.StatusBadGateway, "credential store unavailable")
		}
		return
	}

	result := s.probe(r, req.URL, key, secret.Data[key])
	writeJSON(w, http.StatusOK, result)
	s.cacheResult(r.Context(), team, result.OK)
}

// probe runs the provider probe and folds every outcome — including an empty
// or malformed stored credential — into the {ok, detail} answer. A bad
// credential is a FAILED TEST (200 + ok=false), never a 5xx: the wizard asked
// "does this work?" and the honest answer is no, with the step that failed.
func (s *RepoAuthTestService) probe(r *http.Request, repoURL, key string, token []byte) repoAuthTestResult {
	if len(token) == 0 {
		return repoAuthTestResult{OK: false, Detail: fmt.Sprintf("stored credential has no material under key %q", key)}
	}
	login, repoCount, err := s.prober.Probe(r.Context(), repoURL, string(token))
	if err != nil {
		return repoAuthTestResult{OK: false, Detail: probeDetail(err)}
	}
	return repoAuthTestResult{
		OK:     true,
		Detail: fmt.Sprintf("authenticated as %s; %d repositories visible", login, repoCount),
	}
}

// probeDetail maps a provider/transport error onto the console-renderable
// detail string. It names the failing step and the provider's status — never
// the token, and never the raw Go error (which can embed request context).
func probeDetail(err error) string {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusUnauthorized:
			return "provider rejected the credential (401 unauthorized)"
		case http.StatusForbidden:
			return "provider refused the credential (403 forbidden — check the token's scopes)"
		default:
			return fmt.Sprintf("provider answered %d: %s", ghErr.Response.StatusCode, ghErr.Message)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider probe timed out"
	}
	return "provider probe failed"
}

// cacheResult records the last repo test outcome on the Team CR (AD-2
// annotation cache) on a detached context — a client disconnect right after
// the probe must not cancel the cache write. Best-effort by design: a write
// failure logs and moves on, the probe answer is already sent.
func (s *RepoAuthTestService) cacheResult(ctx context.Context, team *ksquadv1.Team, passed bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	SetRepoTestConnectionFlag(team, passed)
	if err := s.client.Update(ctx, team); err != nil {
		log.Printf("apiserver: repo-auth test cache write failed for team %s: %v", team.Name, err)
	}
}

// resolveTeam resolves the caller's Team UID to the Team object AND its squad
// namespace — one cluster-wide list serves both (the §12.1 resolution
// secretwrite.go performs, plus the in-memory object the annotation write
// needs, so the probe never lists twice).
func (s *RepoAuthTestService) resolveTeam(ctx context.Context, teamUID string) (*ksquadv1.Team, string, error) {
	if teamUID == "" {
		return nil, "", ErrTeamNamespaceUnresolved
	}
	var teams ksquadv1.TeamList
	if err := s.client.List(ctx, &teams); err != nil {
		return nil, "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID && teams.Items[i].Status.Namespace != "" {
			return &teams.Items[i], teams.Items[i].Status.Namespace, nil
		}
	}
	return nil, "", ErrTeamNamespaceUnresolved
}
