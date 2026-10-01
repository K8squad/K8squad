package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// GithubIssueMirrorReader reads the already-mirrored body + comments of one
// GitHub issue from the §5.4 scm mirror so the bridge can mint a ticket carrying
// the REAL upstream issue instead of a stub placeholder (ISI-5308 WS-D.1). It is
// a PURE LOCAL read of scm.mirror_record — written by the operator repo-sync
// relay, which is the ONLY component that holds the PAT — so the apiserver never
// calls GitHub and the credential never enters this path (the investigation on
// ISI-5279: the apiserver SA holds secrets:create, not secrets:get, and cannot
// fetch GitHub itself). The seam is optional: a host without it (or a mirror
// that has not caught up) falls back to the stub body, never a hard failure.
type GithubIssueMirrorReader interface {
	// MirroredIssue returns the mirrored issue addressed by (project CR
	// namespace/name, issue number). ok=false means the mirror has no row for it
	// yet — the caller keeps the stub body and the real one fills in on a later
	// sync. An error is a genuine read failure (DB down), not a cache miss.
	MirroredIssue(ctx context.Context, projectNamespace, projectName, number string) (mi MirroredIssue, ok bool, err error)
}

// MirroredIssue is the slice of the §5.4 mirror the bridge imports: the external
// issue body + its comment thread, already captured locally. EXTERNAL, untrusted
// content (D8) — the imported ticket body carries it verbatim but marks it as
// mirrored upstream content, never as instructions.
type MirroredIssue struct {
	Body     string
	URL      string
	Title    string
	Comments []MirroredIssueComment
}

// MirroredIssueComment is one mirrored issue comment the bridge imports.
type MirroredIssueComment struct {
	Actor     string
	Body      string
	CreatedAt time.Time
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
func githubIssueDispatchHandler(store GithubIssueDispatcher, refs ProjectRefResolver, mirror GithubIssueMirrorReader) http.HandlerFunc {
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
		projectNS, projectName := "", ""
		if refs != nil {
			resolved, err := refs.ResolveProjectRef(r.Context(), projectID)
			if mapProjectRefError(w, err) {
				return
			}
			projectID = resolved.UID
			projectTeam = resolved.TeamUID
			projectNS, projectName = resolved.Namespace, resolved.Name
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
		// The minted ticket body: prefer the REAL upstream issue (body + comments)
		// already mirrored locally (ISI-5308), falling back to the stub when the
		// mirror has not caught up — the ticket is always created, the agent always
		// gets the link, and the real content fills in on a repeat click or when
		// the context assembler reads the mirror directly at run time.
		body := stubIssueBody(req.URL)
		if mirror != nil && projectNS != "" && projectName != "" {
			if mi, ok, err := mirror.MirroredIssue(r.Context(), projectNS, projectName, number); err == nil && ok {
				body = importedIssueBody(req.URL, mi)
			}
			// A mirror read error is non-fatal: the stub body still mints a valid,
			// idempotent ticket, so a transient DB hiccup never blocks a dispatch.
		}

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

// stubIssueBody is the minimal minted-ticket body used before the §5.4 mirror
// has captured the real upstream issue (ISI-5308 fallback): the link plus the
// bridge provenance note. It is the pre-ISI-5308 behaviour, preserved verbatim
// so a store-less / not-yet-mirrored dispatch is unchanged.
func stubIssueBody(url string) string {
	return "Imported from GitHub issue: " + url +
		"\n\nAssigned to an agent from the console GitHub Issues board (ISI-4749). " +
		"At the end of the run the agent posts its result back to the ticket and the linked GitHub issue."
}

// bridgeImportedCommentCap bounds how many mirrored comments the minted ticket
// body carries, so one long thread cannot bloat coord.work_item.description. The
// full thread still reaches the agent through the untrusted-external GitHubDetails
// context element (ISI-5279); this is the human-facing ticket summary.
const bridgeImportedCommentCap = 20

// importedIssueBody builds the minted-ticket body from the mirrored upstream
// issue (ISI-5308): the real issue body followed by a bounded, clearly-fenced
// rendering of its comment thread, then the bridge provenance note. The upstream
// content is EXTERNAL and is labelled as such so a reader (human or agent) treats
// it as mirrored reference, never as instructions (D8). No credential is involved
// — every byte here came from the local mirror.
func importedIssueBody(url string, mi MirroredIssue) string {
	var b strings.Builder
	b.WriteString("Imported from GitHub issue: ")
	b.WriteString(url)
	b.WriteString("\n\n")
	if body := strings.TrimSpace(mi.Body); body != "" {
		b.WriteString("--- Upstream issue (mirrored, external content) ---\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if n := len(mi.Comments); n > 0 {
		shown := mi.Comments
		if n > bridgeImportedCommentCap {
			shown = shown[:bridgeImportedCommentCap]
		}
		fmt.Fprintf(&b, "\n--- Comments (%d, mirrored external content) ---\n", n)
		for _, c := range shown {
			who := c.Actor
			if who == "" {
				who = "unknown"
			}
			when := ""
			if !c.CreatedAt.IsZero() {
				when = " (" + c.CreatedAt.UTC().Format(time.RFC3339) + ")"
			}
			fmt.Fprintf(&b, "\n@%s%s:\n%s\n", who, when, strings.TrimSpace(c.Body))
		}
		if n > len(shown) {
			fmt.Fprintf(&b, "\n… %d more comment(s) omitted; see the linked issue.\n", n-len(shown))
		}
	}
	b.WriteString("\nAssigned to an agent from the console GitHub Issues board (ISI-4749). " +
		"At the end of the run the agent posts its result back to the ticket and the linked GitHub issue.")
	return b.String()
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
