package apiserver

// GitHub sync-trigger write surface (ISI-4011, ADR-0013 §Open Q3). The "Sync
// now" button bumps the ksquad.io/scm-sync-trigger annotation on the Project
// CR — the same path used by the scm-webhook ingress — so the reposync
// reconciler fires immediately without a direct GitHub call.
//
// Design choices:
//   - Writer uses a full client.Client (Patch) — the read model (GithubStatusService)
//     only needs a client.Reader; a separate writer avoids widening that seam.
//   - Per-project debounce: the sync.Map stores the last-trigger Unix-nanosecond
//     timestamp; calls within debounceWindow (30 s) return 429 without a patch
//     (ADR-0013 §Open Q3: avoid webhook-storm-by-button).
//   - RBAC: contributor+ required (the same gate as work-item creates). Wired in
//     server.go via requireProjectRole(Contributor).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/controller/reposync"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const debounceWindow = 30 * time.Second

// GithubSyncService is the "Sync now" write surface. It resolves the Project
// via the SHARED resolution spine (same tenancy as the read model) and patches
// the TriggerAnnotation — never touches GitHub directly.
type GithubSyncService struct {
	reader  client.Reader
	writer  client.Client
	mu      sync.Mutex
	lastHit map[string]time.Time // key: "namespace/name"
}

// NewGithubSyncService constructs the service. Both reader and writer are
// required; nil writer ⇒ caller leaves opts.GithubSync nil and the route
// answers 501.
func NewGithubSyncService(reader client.Reader, writer client.Client) *GithubSyncService {
	return &GithubSyncService{
		reader:  reader,
		writer:  writer,
		lastHit: make(map[string]time.Time),
	}
}

// TriggerSync bumps the scm-sync-trigger annotation on the Project. Returns
// errDebounced when called again within debounceWindow for the same project.
func (s *GithubSyncService) TriggerSync(ctx context.Context, auth discussion.AuthorContext, projectID string) error {
	var ns, name string
	var err error
	if auth.IsAdmin {
		ns, name, err = resolveProjectFleetWide(ctx, s.reader, projectID)
	} else {
		ns, name, err = resolveProjectInTeam(ctx, s.reader, auth.TeamID.String(), projectID)
	}
	if err != nil {
		return err
	}

	key := ns + "/" + name
	s.mu.Lock()
	if last, ok := s.lastHit[key]; ok && time.Since(last) < debounceWindow {
		remaining := debounceWindow - time.Since(last)
		s.mu.Unlock()
		return &debounceError{remaining: remaining}
	}
	s.lastHit[key] = time.Now()
	s.mu.Unlock()

	// Fetch current Project so we can set resourceVersion (optimistic concurrency).
	var proj ksquadv1.Project
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &proj); err != nil {
		return err
	}

	patch := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:            proj.Name,
			Namespace:       proj.Namespace,
			ResourceVersion: proj.ResourceVersion,
			Annotations: map[string]string{
				reposync.TriggerAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	return s.writer.Patch(ctx, patch, client.MergeFrom(&proj))
}

// debounceError carries the remaining window so the handler can set Retry-After.
type debounceError struct{ remaining time.Duration }

func (e *debounceError) Error() string {
	return fmt.Sprintf("sync debounced: retry in %.0fs", e.remaining.Seconds())
}

// ============================================================================
// Handler
// ============================================================================

// projectGithubSync is the POST /api/projects/{projectId}/github/sync handler.
// BFFAuthz + requireProjectRole(Contributor) have already run; we just resolve
// + patch. 202 Accepted on success (the reconciler fires asynchronously).
func (s *Server) projectGithubSync(svc *GithubSyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		projectID := mux.Vars(r)["projectId"]
		err := svc.TriggerSync(r.Context(), auth, projectID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "triggered"})
		case isDebounce(err):
			de := err.(*debounceError)
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", de.remaining.Seconds()))
			writeJSONError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, ErrTeamNotFound), errors.Is(err, ErrProjectNotFound):
			writeJSONError(w, http.StatusNotFound, "project not found")
		case errors.Is(err, ErrProjectAmbiguous):
			writeJSONError(w, http.StatusConflict, "project name ambiguous; address by uid")
		default:
			writeJSONError(w, http.StatusBadGateway, "sync trigger failed")
		}
	}
}

func isDebounce(err error) bool {
	var de *debounceError
	return errors.As(err, &de)
}
