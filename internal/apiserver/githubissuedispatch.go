package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ============================================================================
// GitHub-issue → work-item ASSIGN-&-DISPATCH bridge (ISI-4783 / ISI-4749 Epic 2,
// contract ISI-4757) — POST /api/projects/{projectId}/github/issues/{number}/assign.
// ============================================================================
//
// The GitHub Issues board (console, Epic 1/3) has no write path of its own: a
// GitHub issue is not a coord work item, so "assign this issue to an agent" has
// to MINT the equivalent k8squad ticket first. This handler is the missing write
// half the Epic-3 popup already calls (assignAndDispatch → this route via its BFF);
// without it the apiserver answers 404 and the UI shows "Couldn't find this issue
// to assign." (ISI-4793).
//
// WHAT IT DOES, in ONE call the operator sees as atomic:
//   1. find-or-create the work item that mirrors this GitHub issue, keyed on the
//      idempotency label `ksquad.github.issue=owner/repo#N` (Epic-2 contract), with
//      the issue link + a short body — reusing coord.EnsureReviewWorkItem, the
//      generic "idempotent create-by-label in backlog" primitive (ISI-4766);
//   2. dispatch the human's chosen agent onto it — reusing coord.RequestDispatch,
//      the ADR-0022 board verb (advance backlog→todo + stamp requested_agent), which
//      also enforces the agent-∈-Team check the console cannot.
//
// The two-step is deliberately idempotent and self-healing: EnsureReviewWorkItem
// dedups on the label so a repeat click reuses the row (created:false), and
// RequestDispatch on that reused row either self-heals a still-backlog item (a
// prior pass that created but failed to dispatch), re-assigns an unclaimed todo,
// or returns a clean 409 once a run has claimed it. So we ALWAYS run both steps.
//
// HONESTY (ADR-0013): this is a Paperclip-side dispatch ONLY. It never writes a
// GitHub assignee. The end-of-run write-back TO the GitHub issue is the agent's job
// at run completion (out of scope here — the ticket carries the issue link so the
// agent knows where to comment).
//
// RBAC mirrors the create/dispatch wall exactly: human-only (an agent-authored
// AuthorContext is 403 — agents progress via custody, never by dispatching the
// board); project-scoped, so the route also carries requireProjectRole(Contributor)
// at the wall (server.go); Team scope is server-derived (the resolved Project's
// owning Team), never trusted from the body.

// GithubIssueDispatcher is the two coord ops this endpoint composes. The interface
// (not the concrete stores) is the seam so the host can leave the route
// documented-501 without a DB/cache, and tests can inject a fake. Both methods are
// already shipped: EnsureReviewWorkItem on *coord.WorkItemWriteStore, RequestDispatch
// on *coord.WorkItemDispatchStore (main.go composes the two into one value).
type GithubIssueDispatcher interface {
	EnsureReviewWorkItem(ctx context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error)
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// githubIssueLabelPrefix keys the dedup/join label the whole Epic-2/Epic-4 bridge
// agrees on (ISI-4757): `ksquad.github.issue=owner/repo#N`. It is BOTH the
// idempotency key for the create and the join key Epic-4's local-agent badge reads
// back via FindWorkItemByLabel (ISI-4770).
const githubIssueLabelPrefix = "ksquad.github.issue="

// ghIssueURLRe pulls `owner/repo` and the issue number out of a canonical GitHub
// issue URL (https://github.com/owner/repo/issues/123). The console sends the
// issue's html URL verbatim; we derive the compact ref from it rather than trust a
// separately-supplied ref, so the label is always consistent with the real issue.
var ghIssueURLRe = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/issues/(\d+)`)

// githubIssueAssignRequest is the POST body the Epic-3 control sends: the chosen
// agent NAME (ISI-4501 dispatch convention) and the issue's GitHub URL. Title is
// optional enrichment — used for the minted ticket when present, else derived.
type githubIssueAssignRequest struct {
	AgentID string `json:"agentId"`
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
}

// githubIssueAssignResponse is the Epic-2 result the console renders (ISI-4757
// contract): the minted/looked-up work item, the compact issue ref the apiserver
// derived + labelled, whether THIS call created the item, and the dispatch outcome.
type githubIssueAssignResponse struct {
	WorkItemID string                       `json:"workItemId"`
	IssueRef   string                       `json:"issueRef"`
	Created    bool                         `json:"created"`
	Dispatch   coord.WorkItemDispatchResult `json:"dispatch"`
}

// githubIssueDispatchHandler answers POST
// /api/projects/{projectId}/github/issues/{number}/assign.
func githubIssueDispatchHandler(store GithubIssueDispatcher, refs ProjectRefResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		// RBAC: human-only. Agents progress through custody, not by dispatching.
		if auth.AgentID != nil {
			writeJSONError(w, http.StatusForbidden, "github-issue dispatch is human-only; agents progress via custody")
			return
		}
		projectID, ok := pathVar(r, "projectId")
		if !ok || projectID == "" {
			writeJSONError(w, http.StatusBadRequest, "project id required")
			return
		}
		number, ok := pathVar(r, "number")
		if !ok || number == "" {
			writeJSONError(w, http.StatusBadRequest, "issue number required")
			return
		}

		// Resolve the console's project ref to the CR identity + owning Team, exactly
		// like the human work-item create (workitemwrite.go): the Project's owning
		// Team is the minted ticket's tenancy, so it is immediately dispatchable.
		projectTeam := ""
		if refs != nil {
			resolved, err := refs.ResolveProjectRef(r.Context(), projectID)
			if mapProjectRefError(w, err) {
				return
			}
			projectID = resolved.UID
			projectTeam = resolved.TeamUID
		}

		var req githubIssueAssignRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.AgentID == "" {
			writeJSONError(w, http.StatusBadRequest, "agentId required")
			return
		}

		issueRef, err := deriveGithubIssueRef(req.URL, number)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		dedupLabel := githubIssueLabelPrefix + issueRef

		// Team scope: the resolved Project's owning Team wins; without a resolver the
		// caller's Team is used, and a fleet-admin with no bound Team is refused
		// honestly (an item with no team can never mint a Run — intake requires it).
		teamID := projectTeam
		if teamID == "" && auth.TeamID.String() != "00000000-0000-0000-0000-000000000000" {
			teamID = auth.TeamID.String()
		}
		if teamID == "" {
			writeJSONError(w, http.StatusBadRequest, "fleet-admin github-issue dispatch requires a team-scoped project (ISI-3937)")
			return
		}

		title := req.Title
		if title == "" {
			title = "GitHub issue " + issueRef
		}
		body := "Imported from GitHub issue: " + req.URL +
			"\n\nAssigned to an agent from the console GitHub Issues board (ISI-4749). " +
			"At the end of the run the agent posts its result back to the ticket and the linked GitHub issue."

		// (1) find-or-create the mirror ticket, idempotent on the issue label.
		ens, err := store.EnsureReviewWorkItem(r.Context(), coord.EnsureReviewWorkItemInput{
			ProjectID:  projectID,
			TeamID:     teamID,
			Title:      title,
			Body:       body,
			DedupLabel: dedupLabel,
			Principal:  auth.Principal,
		})
		if mapWorkItemWriteError(w, err) {
			return
		}

		// (2) dispatch the chosen agent onto it. On a reused row this self-heals a
		// still-backlog item, re-assigns an unclaimed todo, or 409s once claimed —
		// so a repeat click is always safe (created:false + idempotent dispatch).
		disp, err := store.RequestDispatch(r.Context(), coord.RequestDispatchInput{
			WorkItemID: ens.Item.ID,
			AgentID:    req.AgentID,
			TeamID:     teamID, // server-derived, never from the body
			Principal:  auth.Principal,
		})
		if mapWorkItemWriteError(w, err) {
			return
		}

		writeJSON(w, http.StatusOK, githubIssueAssignResponse{
			WorkItemID: ens.Item.ID,
			IssueRef:   issueRef,
			Created:    ens.Created,
			Dispatch:   disp,
		})
	}
}

// deriveGithubIssueRef builds the compact `owner/repo#N` ref from the issue's
// GitHub URL, cross-checking the number against the {number} path segment so a
// mismatched body can never mislabel the minted ticket. A URL that does not parse
// is a 400 (we refuse to fabricate a label we can't tie to a real issue).
func deriveGithubIssueRef(url, pathNumber string) (string, error) {
	m := ghIssueURLRe.FindStringSubmatch(url)
	if m == nil {
		return "", fmt.Errorf("could not derive issue ref from url %q", url)
	}
	repo, urlNumber := m[1], m[2]
	// The path is the authoritative number (the URL is caller-supplied); require
	// them to agree so a stale/foreign URL cannot relabel the wrong issue.
	if n, err := strconv.Atoi(pathNumber); err != nil || strconv.Itoa(n) != urlNumber {
		return "", fmt.Errorf("issue number %q does not match url %q", pathNumber, url)
	}
	return repo + "#" + urlNumber, nil
}
