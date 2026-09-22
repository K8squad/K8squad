// lib/github-links.ts — canonical GitHub deep-link helpers shared by the GitHub
// integration screens (ISI-4662 children).
//
// Every actionable entity on a GitHub screen links back to the real repo so the
// operator can act in GitHub directly (board-approved v2 design). The mirror
// carries a normalized `url` for most entities; when it is absent we reconstruct
// the canonical GitHub path from the identifier. The repo slug is a constant
// (the console surfaces the K8squad repo) — never derived from untrusted input.

export const GITHUB_REPO = "K8squad/K8squad";
export const GITHUB_REPO_URL = `https://github.com/${GITHUB_REPO}`;
export const GITHUB_REPO_LABEL = "k8squad/k8squad";

/** Percent-encode a path-like identifier while preserving `/` separators, so
 * branch names such as `feat/x` produce the canonical `tree/feat/x` path. */
function encodePath(value: string): string {
  return value.split("/").map(encodeURIComponent).join("/");
}

/** `releases/tag/{tag}` — prefers the mirror-normalized release URL. */
export function githubReleaseHref(tag: string, mirrored?: string): string {
  if (mirrored) return mirrored;
  return `${GITHUB_REPO_URL}/releases/tag/${encodePath(tag)}`;
}

/** `tree/{branch}` — the branch's tree view. The mirrored branch URL points at
 * the head commit, so the tree path is always reconstructed from the name. */
export function githubBranchHref(branch: string): string {
  return `${GITHUB_REPO_URL}/tree/${encodePath(branch)}`;
}

/** `pull/{n}` — prefers the mirror-normalized PR URL. */
export function githubPullHref(number: number, mirrored?: string): string {
  if (mirrored) return mirrored;
  return `${GITHUB_REPO_URL}/pull/${number}`;
}

/** `issues/{n}` — prefers the mirror-normalized issue URL. */
export function githubIssueHref(number: number, mirrored?: string): string {
  if (mirrored) return mirrored;
  return `${GITHUB_REPO_URL}/issues/${number}`;
}

/** `actions/runs/{id}` — prefers the mirror-normalized run URL. */
export function githubRunHref(id: string | number, mirrored?: string): string {
  if (mirrored) return mirrored;
  return `${GITHUB_REPO_URL}/actions/runs/${encodeURIComponent(String(id))}`;
}

/** `/projects/{projectId}/issues/{workItemId}` — the IN-CONSOLE deep-link to a
 * work item (ISI-4767 E5). Unlike the GitHub helpers above this stays inside the
 * console (the ticket/overview surface, ISI-4707 path route), so a PR-card review
 * badge can jump straight to the review work item. Returns "" when either id is
 * missing so the caller can render presence-only text rather than a dead link. */
export function reviewWorkItemHref(projectId: string, workItemId: string): string {
  if (!projectId || !workItemId) return "";
  return `/projects/${encodeURIComponent(projectId)}/issues/${encodeURIComponent(workItemId)}`;
}
