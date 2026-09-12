/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Run kill API (story 3.3 + 8.4, ISI-2884) — POST /api/work-items/{id}/kill.
// ============================================================================
//
// The console's ≤2-click kill lands here. The apiserver NEVER writes the Run
// CR (status is the projector's, spec is the composer's — §5.1): kill is
// recorded on the DURABLE machine, fence-first (CancelEnter on the coord
// claim), and the operator's drive loop + kill sweep do the teardown and the
// terminal finish (cancelling → cancelled). The work-item key (not the Run
// name) is the contract: it is the coord claim key and the value every read
// model already carries (overview rows, artifact browser) — names are
// ambiguous across namespaces, work-item ids are not.

// ErrKillConflict reports the fence moved under the kill (a retry lap or
// another kill raced). The caller re-reads and retries — never blind-forces.
var ErrKillConflict = errors.New("apiserver: kill conflicted with a concurrent transition")

// ErrKillNotFound reports no coord claim exists for the work item (dangling
// ref or not-yet-enrolled Run).
var ErrKillNotFound = errors.New("apiserver: no claim for work item")

// RunKiller is the kill seam: the production binding is the coord
// ProdCancelStore (CancelEnter) over the host DB; tests wire a fake. Kill is
// idempotent-by-outcome: a Run already cancelling/terminal reports its state
// without inventing a second transition.
type RunKiller interface {
	// Kill issues the fence-first cancel-enter for the work item.
	// initiatedBy stamps the audit row's initiated_by_user_id (§6.5 — a kill
	// is human-initiated): the initiating user's auth.user id (uuid), or ""
	// when no user is resolved (NULL). Never a principal string — the column
	// is uuid (ISI-4299 defect 2).
	Kill(ctx context.Context, workItemID, initiatedBy string) (phase string, err error)
}

// killRunHandler answers POST /api/work-items/{workItemId}/kill. It rides the
// §13 BFF authz choke point, so the AuthorContext (principal) is already
// stamped. The audit's initiated_by_user_id is the RESOLVED auth.user id —
// the principal is a stable string key, not a uuid, and the uuid column
// rejects it (ISI-4299); a session-less initiator degrades to NULL, with the
// acting server principal still on the audit row.
func killRunHandler(killer RunKiller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id := mux.Vars(r)["workItemId"]
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, "work item id is required")
			return
		}
		phase, err := killer.Kill(r.Context(), id, auth.UserID)
		switch {
		case errors.Is(err, ErrKillConflict):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "conflict", "detail": "a concurrent transition raced the kill; retry",
				"phase": phase,
			})
		case errors.Is(err, ErrKillNotFound):
			writeJSONError(w, http.StatusNotFound, "no run claim for that work item")
		case err != nil:
			writeJSONError(w, http.StatusBadGateway, "kill seam unavailable")
		default:
			writeJSON(w, http.StatusOK, map[string]string{
				"status":     "kill issued",
				"workItem":   id,
				"phase":      phase,
				"transition": "Canceling",
			})
		}
	}
}
