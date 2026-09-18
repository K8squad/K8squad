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

/** `releases/tag/{tag}` — prefers the mirror-normalized release URL. */
export function githubReleaseHref(tag: string, mirrored?: string): string {
  if (mirrored) return mirrored;
  return `${GITHUB_REPO_URL}/releases/tag/${encodeURIComponent(tag)}`;
}

/** `tree/{branch}` — the branch's tree view. The mirrored branch URL points at
 * the head commit, so the tree path is always reconstructed from the name. */
export function githubBranchHref(branch: string): string {
  return `${GITHUB_REPO_URL}/tree/${encodeURIComponent(branch)}`;
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
