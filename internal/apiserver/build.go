package apiserver

import (
	"errors"
	"net/http"

	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/gorilla/mux"
)

// buildHandler serves GET /api/runs/{runId}/build/{resource} (8.7a read-model behind the 8.7d gate,
// ISI-2759). It is mounted behind the §13 BFFAuthz choke point, so the AuthorContext is already on
// the request context; this handler projects it to a buildbrowser.Caller and never trusts the body.
//
// The four resources — tree | diff | file | meta — dispatch into the build-browser Service, which
// applies the 8.7d per-principal + Team-scope gate BEFORE any git read. Every deny-or-missing path
// collapses to 404 (existence-hiding): a same-Team non-owner cannot tell "forbidden" from "no such
// Run", matching the BFF route's contract (a 404 stays a 404, never re-mapped to 403).
func buildHandler(svc *buildbrowser.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			// Defense in depth: BFFAuthz should have rejected this already.
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		caller := buildbrowser.Caller{
			Principal: auth.Principal,
			TeamID:    auth.TeamID,
			IsAdmin:   auth.IsAdmin,
		}

		vars := mux.Vars(r)
		runID := vars["runId"]
		q := r.URL.Query()
		ref := q.Get("ref")

		var (
			payload any
			err     error
		)
		switch vars["resource"] {
		case "tree":
			payload, err = svc.Tree(r.Context(), caller, runID, ref)
		case "diff":
			payload, err = svc.Diff(r.Context(), caller, runID)
		case "file":
			payload, err = svc.File(r.Context(), caller, runID, ref, q.Get("path"))
		case "meta":
			payload, err = svc.Meta(r.Context(), caller, runID)
		default:
			// Unknown resource is indistinguishable from a missing Run (existence-hiding).
			http.NotFound(w, r)
			return
		}

		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, payload)
		case errors.Is(err, buildbrowser.ErrNotFound):
			http.NotFound(w, r) // 404 — deny and not-found are the same answer (8.7d)
		case errors.Is(err, buildbrowser.ErrBadRequest):
			writeJSONError(w, http.StatusBadRequest, "invalid request")
		default:
			writeJSONError(w, http.StatusInternalServerError, "build read-model error")
		}
	}
}
