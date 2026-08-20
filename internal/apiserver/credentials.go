package apiserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Credential / auth-state read model (story 8.6 / ISI-2902) — the projection
// GET /api/credentials answers for the console's Credentials screen.
// ============================================================================
//
// Source of truth is the controller-runtime informer cache (same discipline as overview.go):
// Agents and Runs are CRDs, so the per-agent credential surface (BYO Secret ref, runtime, and any
// Paused-on-credential Runs) is the cache's level-triggered projection of etcd. The projection is
// Team-SCOPED (AuthorContext.TeamID → Team namespace, §12.1 tenancy root) — a caller never sees
// another squad's credential rows.
//
// Health derivation (8.6 AC + CEO 2026-08-12): per-agent health is *connected / refreshing /
// expired*, derived from the Run conditions the reconciler writes (run_types.go documents
// Paused(auth_failure) / Paused(rate_limited); Epic 7.4 adds the credential reason family). The
// screen must show a CLEAR paused-on-expiry signal — so a Paused Run whose condition reason names
// a credential failure marks that Agent's row expired·paused and feeds the banner. What the cache
// CANNOT see (token expiry horizons, refresh progress — the 7.7 credential controller's Secret
// writes) is surfaced as unknown rather than fabricated: the console never renders a made-up
// number (same no-placeholder discipline as 8.8b / FR-I3).
//
// FR-G1 footer fact is structural: every row is a per-user Secret ref read off the Agent spec;
// there is no master-credential field anywhere to project.

// Health states for one Agent's credential (8.6 CEO 2026-08-12: connected / refreshing / expired).
const (
	CredHealthConnected  = "connected"
	CredHealthRefreshing = "refreshing"
	CredHealthExpired    = "expired"
	CredHealthUnknown    = "unknown"
)

// CredentialCondition reasons (metav1.Condition.Reason) that mark a credential hold. The
// reconciler's family per arch §8/§10: auth failures and credential expiry pause the Run; the
// exact vocabulary is pinned by Epic 7.4/ISI-2898 (credential_expired / credential_rotated /
// endpoint_unreachable) and K8s convention may PascalCase it (CredentialExpired) — matched on a
// case/separator-normalized token so a casing change upstream needs no console change.
var credentialHoldReasons = map[string]bool{
	"authfailure":         true,
	"credexpired":         true,
	"credentialexpired":   true,
	"credentialrotated":   true,
	"endpointunreachable": true,
}

func isCredentialHold(reason string) bool {
	var b []byte
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		switch r {
		case '_', '-', ' ', '.':
			continue
		default:
			b = append(b, byte(r))
		}
	}
	return credentialHoldReasons[string(b)]
}

// PausedRunRef is one Run paused on a credential hold, for the row's RUNS cell and the banner's
// deep link (run detail 8.11 / stream 8.2).
type PausedRunRef struct {
	Name   string     `json:"name"`
	Reason string     `json:"reason"`
	Since  *time.Time `json:"since,omitempty"`
}

// AgentCredentialRow is one row of the credentials table (mock 05): agent identity, runtime,
// credential Secret ref, token class, expiry horizon, health, and any paused Runs.
type AgentCredentialRow struct {
	Agent           string         `json:"agent"`
	Namespace       string         `json:"namespace"`
	Runtime         string         `json:"runtime"`
	Model           string         `json:"model,omitempty"`
	CredentialRef   string         `json:"credentialRef"` // "namespace/name" of the per-user Secret
	CredentialClass string         `json:"credentialClass,omitempty"`
	ExpiresAt       *time.Time     `json:"expiresAt,omitempty"` // nil ⇒ unknown (no controller data)
	ExpiresKnown    bool           `json:"expiresKnown"`
	Health          string         `json:"health"` // connected|refreshing|expired|unknown
	PausedRuns      []PausedRunRef `json:"pausedRuns,omitempty"`
}

// CredentialsOverview is the GET /api/credentials payload: the Team-scoped agent rows plus the
// Connect-Claude surface state (7.7 one-click OAuth — its backing lands with ISI-2899; until then
// the field is honestly false and the button renders its not-configured state).
type CredentialsOverview struct {
	Team          string               `json:"team"`
	Agents        []AgentCredentialRow `json:"agents"`
	ConnectClaude bool                 `json:"connectClaude"`
}

// CredentialOverviewReader projects the per-agent credential surface for a single Team (by K8s
// object UID — the AuthorContext.TeamID). Production wires the cache-backed reader; tests wire a
// fake client.Reader. A reader MUST scope strictly to teamUID.
type CredentialOverviewReader interface {
	Credentials(ctx context.Context, teamUID string) (CredentialsOverview, error)
}

// ClientCredentialReader is the production CredentialOverviewReader over any client.Reader (the
// informer cache in the host; a fake client in tests). Read-only.
type ClientCredentialReader struct {
	reader client.Reader
}

// NewClientCredentialReader builds the read model over a client.Reader.
func NewClientCredentialReader(r client.Reader) *ClientCredentialReader {
	return &ClientCredentialReader{reader: r}
}

// Credentials resolves the Team by UID (same rename-proof scoping as overview.go), then projects
// every Agent in the Team's namespace into a credential row, joining Paused Runs whose
// spec.agents select that Agent. Deterministic: rows sort by agent name, paused runs by name.
func (r *ClientCredentialReader) Credentials(ctx context.Context, teamUID string) (CredentialsOverview, error) {
	ns, err := r.teamNamespace(ctx, teamUID)
	if err != nil {
		return CredentialsOverview{}, err
	}

	var agents ksquadv1.AgentList
	if err := r.reader.List(ctx, &agents, client.InNamespace(ns)); err != nil {
		return CredentialsOverview{}, err
	}
	var runs ksquadv1.RunList
	if err := r.reader.List(ctx, &runs, client.InNamespace(ns)); err != nil {
		return CredentialsOverview{}, err
	}

	// Index paused credential holds by the Agents the paused Run selects.
	holdsByAgent := map[string][]PausedRunRef{}
	for i := range runs.Items {
		run := &runs.Items[i]
		hold, ok := credentialHold(run)
		if !ok {
			continue
		}
		for _, ref := range run.Spec.Agents {
			holdsByAgent[ref.Name] = append(holdsByAgent[ref.Name], hold)
		}
	}

	out := CredentialsOverview{Team: ns}
	for i := range agents.Items {
		a := &agents.Items[i]
		row := AgentCredentialRow{
			Agent:         a.Name,
			Namespace:     a.Namespace,
			Runtime:       a.Spec.RuntimeRef.Name,
			Model:         a.Spec.Model,
			CredentialRef: a.Namespace + "/" + a.Spec.CredentialSecretRef.Name,
			Health:        CredHealthConnected,
		}
		if cls, ok := a.Annotations["ksquad.io/credential-class"]; ok {
			row.CredentialClass = cls
		}
		if holds, held := holdsByAgent[a.Name]; held {
			sort.Slice(holds, func(x, y int) bool { return holds[x].Name < holds[y].Name })
			row.PausedRuns = holds
			row.Health = CredHealthExpired
		}
		out.Agents = append(out.Agents, row)
	}
	sort.Slice(out.Agents, func(x, y int) bool { return out.Agents[x].Agent < out.Agents[y].Agent })
	return out, nil
}

// teamNamespace resolves the Team UID to its namespace (the §12.1 tenancy root). Shared shape
// with overview.go's inline resolution, factored here because both read models scope by it.
func (r *ClientCredentialReader) teamNamespace(ctx context.Context, teamUID string) (string, error) {
	if teamUID == "" {
		return "", ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := r.reader.List(ctx, &teams); err != nil {
		return "", err
	}
	for i := range teams.Items {
		if string(teams.Items[i].UID) == teamUID {
			return teams.Items[i].Namespace, nil
		}
	}
	return "", ErrTeamNotFound
}

// credentialHold extracts the paused-on-credential signal from one Run: phase Paused AND a
// Paused-type condition whose reason names a credential failure. Anything else (rate_limited,
// pending, running) is not a credential hold.
func credentialHold(run *ksquadv1.Run) (PausedRunRef, bool) {
	if run.Status.Phase != ksquadv1.RunPhasePaused {
		return PausedRunRef{}, false
	}
	for i := range run.Status.Conditions {
		c := &run.Status.Conditions[i]
		if c.Type != "Paused" || c.Status != metav1.ConditionTrue {
			continue
		}
		if isCredentialHold(c.Reason) {
			ref := PausedRunRef{Name: run.Name, Reason: c.Reason}
			if !c.LastTransitionTime.IsZero() {
				t := c.LastTransitionTime.Time
				ref.Since = &t
			}
			return ref, true
		}
	}
	return PausedRunRef{}, false
}

// credentials is the handler behind GET /api/credentials. Rides the §13 BFF authz choke point
// (mounted in routes); the projection is scoped to the caller's Team and reads nothing from the
// request beyond the stamped AuthorContext.
func (s *Server) credentials(reader CredentialOverviewReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		overview, err := reader.Credentials(r.Context(), auth.TeamID.String())
		if errors.Is(err, ErrTeamNotFound) {
			writeJSONError(w, http.StatusNotFound, "no credential surface for this team")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "credential read model unavailable")
			return
		}
		writeJSON(w, http.StatusOK, overview)
	}
}

// connectClaude is the handler behind POST /api/credentials/connect — the 7.7 one-click
// browser-OAuth seam. The OAuth flow itself (login URL minting, callback, Secret write-back) is
// ISI-2899's build; until it lands this answers the DOCUMENTED 501 (house pattern: the route is
// part of the contract, only its backing is pending) so the console button has one honest
// endpoint to call instead of a fabricated flow.
func (s *Server) connectClaude() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":    "not implemented",
			"detail":   "Connect Claude (zero-touch OAuth lifecycle, story 7.7) is not yet hosted by the apiserver",
			"tracking": "ISI-2899: credential controller + Connect Claude OAuth flow",
		})
	}
}
