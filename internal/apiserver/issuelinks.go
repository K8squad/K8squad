package apiserver

// issuelinks.go — Story 11.2 (ISI-2738): the issue⇄work-item linkage API —
// GET/POST/DELETE /api/projects/{projectId}/issue-links behind the same
// §13 BFF choke point as every other gated surface.
//
// WHAT THIS IS: the write path that ESTABLISHES linkage (scm.issue_link,
// migration 0013) and the read path the console badges items from — the
// provenance column (ksquad-native vs external-sourced) is the story-11.2
// AC2 contract "synced state lives in Postgres with provenance tagging so
// the console distinguishes KSquad-native vs GitHub-sourced items". The
// SYNC itself is not driven here: the repo-sync reconciler's level-triggered
// pass (pkg/issuesync wired into pkg/controller/reposync) picks links up on
// the next webhook/poll tick — this API never talks to a provider.
//
// RBAC: the route rides the §13 choke point; tenancy is resolved the same
// way the 8.8a dashboard resolves it (AuthorContext.TeamID → Team UID → the
// §12.1 namespace; a Project outside it is 404, existence-hiding NFR-SEC5).
// Provenance is caller-supplied SEMANTICS, not caller authority: linking an
// item that already exists is ksquad-native; importing from a provider issue
// is external-sourced — both are facts about origin the caller states and
// the schema pins (0013 CHECK).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/issuesync"
)

// ErrIssueLinkBadProvenance mirrors ErrProjectNotFound-style handler errors.
var ErrIssueLinkBadProvenance = errors.New("apiserver: provenance must be ksquad-native or external-sourced")

// IssueLinkStore is the persistence seam this API drives (issuesync.SQLStore
// in prod). The interface (not the concrete store) is the seam so a DB-less
// dev run keeps the documented 501 and tests can inject a fake.
type IssueLinkStore interface {
	ListLinks(ctx context.Context, projectNamespace, projectName string) ([]issuesync.Link, error)
	EstablishLink(ctx context.Context, p issuesync.LinkParams) (issuesync.Link, error)
	DeleteLink(ctx context.Context, projectNamespace, projectName, externalID string) error
}

// IssueLinkService resolves the caller's Team namespace (the §12.1 tenancy
// root) and answers link reads/writes scoped to (namespace, project).
type IssueLinkService struct {
	store  IssueLinkStore
	reader client.Reader
}

// NewIssueLinkService binds the service to the link store and an object
// reader (the apiserver's informer-cache client) for Team/Project lookup.
func NewIssueLinkService(store IssueLinkStore, reader client.Reader) *IssueLinkService {
	return &IssueLinkService{store: store, reader: reader}
}

// linkRequest is the POST body.
type linkRequest struct {
	WorkItemID  string `json:"workItemId"`
	Provider    string `json:"provider"`
	Repo        string `json:"repo"`
	ExternalID  string `json:"externalId"`
	ExternalURL string `json:"externalUrl,omitempty"`
	Direction   string `json:"direction,omitempty"`
	Provenance  string `json:"provenance"`
}

// issueLinkView is the wire shape of one link (console badge source).
type issueLinkView struct {
	WorkItemID     string   `json:"workItemId"`
	Provider       string   `json:"provider"`
	Repo           string   `json:"repo"`
	ExternalID     string   `json:"externalId"`
	ExternalURL    string   `json:"externalUrl,omitempty"`
	Direction      string   `json:"direction"`
	Provenance     string   `json:"provenance"`
	LastWriter     string   `json:"lastWriter"`
	ExternalState  string   `json:"externalState,omitempty"`
	ExternalLabels []string `json:"externalLabels"`
	LastSyncedAt   string   `json:"lastSyncedAt,omitempty"`
}

// List answers GET /api/projects/{projectId}/issue-links.
func (s *IssueLinkService) List(w http.ResponseWriter, r *http.Request) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ns, project, err := s.resolveScope(r, auth)
	if err != nil {
		writeScopeError(w, err)
		return
	}
	links, err := s.store.ListLinks(r.Context(), ns, project)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "issue-link store unavailable")
		return
	}
	out := make([]issueLinkView, 0, len(links))
	for _, l := range links {
		out = append(out, linkView(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

// Establish answers POST /api/projects/{projectId}/issue-links.
func (s *IssueLinkService) Establish(w http.ResponseWriter, r *http.Request) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ns, project, err := s.resolveScope(r, auth)
	if err != nil {
		writeScopeError(w, err)
		return
	}

	var req linkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.WorkItemID == "" || req.Provider == "" || req.Repo == "" || req.ExternalID == "" {
		writeJSONError(w, http.StatusBadRequest, "workItemId, provider, repo and externalId are required")
		return
	}
	if req.Provenance != issuesync.ProvenanceKSquadNative && req.Provenance != issuesync.ProvenanceExternalSourced {
		writeJSONError(w, http.StatusBadRequest, ErrIssueLinkBadProvenance.Error())
		return
	}
	if req.Direction != "" && req.Direction != issuesync.DirectionInbound && req.Direction != issuesync.DirectionBidirectional {
		writeJSONError(w, http.StatusBadRequest, "direction must be inbound or bidirectional")
		return
	}

	link, err := s.store.EstablishLink(r.Context(), issuesync.LinkParams{
		ProjectNamespace: ns,
		ProjectName:      project,
		WorkItemID:       req.WorkItemID,
		Provider:         req.Provider,
		Repo:             req.Repo,
		ExternalID:       req.ExternalID,
		ExternalURL:      req.ExternalURL,
		Direction:        req.Direction,
		Provenance:       req.Provenance,
	})
	var exists *issuesync.ErrLinkExists
	switch {
	case errors.As(err, &exists):
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, issuesync.ErrNoSuchWorkItem):
		writeJSONError(w, http.StatusNotFound, "work item not found")
		return
	case err != nil:
		writeJSONError(w, http.StatusBadGateway, "issue-link store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, linkView(link))
}

// Remove answers DELETE /api/projects/{projectId}/issue-links/{externalId}.
func (s *IssueLinkService) Remove(w http.ResponseWriter, r *http.Request) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ns, project, err := s.resolveScope(r, auth)
	if err != nil {
		writeScopeError(w, err)
		return
	}
	externalID, ok := mux.Vars(r)["externalId"]
	if !ok || externalID == "" {
		writeJSONError(w, http.StatusBadRequest, "externalId required")
		return
	}
	if err := s.store.DeleteLink(r.Context(), ns, project, externalID); err != nil {
		writeJSONError(w, http.StatusNotFound, "no such issue link")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": externalID})
}

// resolveScope resolves (namespace, projectName) from the caller's Team and
// the path's projectId, with the 8.8a existence-hiding tenancy contract.
func (s *IssueLinkService) resolveScope(r *http.Request, auth discussion.AuthorContext) (string, string, error) {
	if auth.TeamID.String() == "" {
		return "", "", ErrTeamNotFound
	}
	var teams ksquadv1.TeamList
	if err := s.reader.List(r.Context(), &teams); err != nil {
		return "", "", err
	}
	ns := ""
	for i := range teams.Items {
		if string(teams.Items[i].UID) == auth.TeamID.String() {
			ns = teams.Items[i].Namespace
			break
		}
	}
	if ns == "" {
		return "", "", ErrTeamNotFound
	}
	project := mux.Vars(r)["projectId"]
	var projects ksquadv1.ProjectList
	if err := s.reader.List(r.Context(), &projects, client.InNamespace(ns)); err != nil {
		return "", "", err
	}
	for i := range projects.Items {
		if projects.Items[i].Name == project {
			return ns, project, nil
		}
	}
	return "", "", ErrProjectNotFound
}

func writeScopeError(w http.ResponseWriter, err error) {
	switch err {
	case ErrTeamNotFound:
		writeJSONError(w, http.StatusNotFound, "no team scope for caller")
	case ErrProjectNotFound:
		writeJSONError(w, http.StatusNotFound, "no project matches the caller's team scope")
	default:
		writeJSONError(w, http.StatusBadGateway, "scope resolution unavailable")
	}
}

func linkView(l issuesync.Link) issueLinkView {
	return issueLinkView{
		WorkItemID:     l.WorkItemID,
		Provider:       l.Provider,
		Repo:           l.Repo,
		ExternalID:     l.ExternalID,
		ExternalURL:    l.ExternalURL,
		Direction:      l.Direction,
		Provenance:     l.Provenance,
		LastWriter:     l.LastWriter,
		ExternalState:  l.ExternalState,
		ExternalLabels: l.ExternalLabels,
		LastSyncedAt:   l.LastSyncedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
