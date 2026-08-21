package apiserver

import (
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/artifactbrowser"
	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
)

// artifactsHandler serves GET /api/runs/{runId}/artifacts (story 8.3 list — ISI-2900). It is
// mounted behind the §13 BFFAuthz choke point, so the AuthorContext is already on the request
// context; this handler projects it to a buildbrowser.Caller (the SAME caller shape the 8.7d
// build-browser gate rides) and never trusts the body.
//
// Existence-hiding is uniform with the build browser: every deny-or-missing path collapses to 404
// so a same-Team non-owner cannot tell "forbidden" from "no such Run" (NFR-SEC5).
func artifactsHandler(svc *artifactbrowser.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerFromContext(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		runID := mux.Vars(r)["runId"]
		listing, err := svc.Listing(r.Context(), caller, runID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, listing)
		case errors.Is(err, artifactbrowser.ErrNotFound):
			http.NotFound(w, r) // 404 — deny and not-found are the same answer
		case errors.Is(err, artifactbrowser.ErrBadRequest):
			writeJSONError(w, http.StatusBadRequest, "invalid request")
		default:
			writeJSONError(w, http.StatusInternalServerError, "artifact read-model error")
		}
	}
}

// artifactContentHandler serves GET /api/runs/{runId}/artifacts/{artifactId} (story 8.3 blob
// view). The artifact is resolved WITHIN the gated Run's rows, so a guessed uuid from another Run
// is indistinguishable from a missing one.
func artifactContentHandler(svc *artifactbrowser.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerFromContext(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		vars := mux.Vars(r)
		res, err := svc.Content(r.Context(), caller, vars["runId"], vars["artifactId"])
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, res)
		case errors.Is(err, artifactbrowser.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, artifactbrowser.ErrBadRequest):
			writeJSONError(w, http.StatusBadRequest, "invalid request")
		default:
			writeJSONError(w, http.StatusInternalServerError, "artifact read-model error")
		}
	}
}

// callerFromContext projects the §13 AuthorContext to the buildbrowser.Caller every Run-scoped
// read-model gate authorizes. Defense in depth: BFFAuthz should have rejected an unresolved
// principal already.
func callerFromContext(r *http.Request) (buildbrowser.Caller, bool) {
	auth, ok := discussion.AuthFromContext(r.Context())
	if !ok || auth.Principal == "" {
		return buildbrowser.Caller{}, false
	}
	return buildbrowser.Caller{
		Principal: auth.Principal,
		TeamID:    auth.TeamID,
		IsAdmin:   auth.IsAdmin,
	}, true
}
