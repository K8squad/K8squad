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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
)

// killCall records one Kill invocation the fake observed.
type killCall struct {
	workItem    string
	initiatedBy string
}

// recordingKiller is the test RunKiller: it captures calls and replays a
// canned phase/error (ISI-4299 — proves the handler passes the RESOLVED user
// uuid, never the principal string, as the audit initiator).
type recordingKiller struct {
	phase string
	err   error
	calls []killCall
}

func (k *recordingKiller) Kill(_ context.Context, workItem, initiatedBy string) (string, error) {
	k.calls = append(k.calls, killCall{workItem: workItem, initiatedBy: initiatedBy})
	return k.phase, k.err
}

func killRequest(auth discussion.AuthorContext, workItemID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/work-items/"+workItemID+"/kill", nil)
	r = mux.SetURLVars(r, map[string]string{"workItemId": workItemID})
	return r.WithContext(discussion.WithAuth(r.Context(), auth))
}

func TestKillRunHandlerStampsResolvedUserID(t *testing.T) {
	userID := uuid.NewString()
	killer := &recordingKiller{phase: "Canceling"}
	w := httptest.NewRecorder()
	killRunHandler(killer).ServeHTTP(w, killRequest(discussion.AuthorContext{
		Principal: "user:admin",
		UserID:    userID,
		TeamID:    uuid.New(),
	}, "11111111-1111-1111-1111-111111111111"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(killer.calls) != 1 {
		t.Fatalf("Kill calls = %d, want 1", len(killer.calls))
	}
	if got := killer.calls[0].initiatedBy; got != userID {
		t.Fatalf("initiatedBy = %q, want the resolved auth.user id %q — a principal string must never reach the uuid audit column", got, userID)
	}
}

func TestKillRunHandlerUnresolvedUserPassesEmpty(t *testing.T) {
	killer := &recordingKiller{phase: "Canceling"}
	w := httptest.NewRecorder()
	killRunHandler(killer).ServeHTTP(w, killRequest(discussion.AuthorContext{
		Principal: "user:admin",
		TeamID:    uuid.New(),
	}, "11111111-1111-1111-1111-111111111111"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := killer.calls[0].initiatedBy; got != "" {
		t.Fatalf("initiatedBy = %q, want \"\" (NULL) when no user id is resolved", got)
	}
}

func TestKillRunHandlerUnauthenticated(t *testing.T) {
	killer := &recordingKiller{}
	r := mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/api/work-items/x/kill", nil),
		map[string]string{"workItemId": "x"})
	w := httptest.NewRecorder()
	killRunHandler(killer).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(killer.calls) != 0 {
		t.Fatalf("Kill calls = %d, want 0 for an unauthenticated request", len(killer.calls))
	}
}

func TestKillRunHandlerErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantCod int
	}{
		{"conflict", ErrKillConflict, http.StatusConflict},
		{"missing", ErrKillNotFound, http.StatusNotFound},
		{"seam", context.DeadlineExceeded, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			killer := &recordingKiller{phase: "Running", err: tc.err}
			w := httptest.NewRecorder()
			killRunHandler(killer).ServeHTTP(w, killRequest(discussion.AuthorContext{
				Principal: "user:admin",
				TeamID:    uuid.New(),
			}, "11111111-1111-1111-1111-111111111111"))
			if w.Code != tc.wantCod {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantCod, w.Body.String())
			}
		})
	}
}
