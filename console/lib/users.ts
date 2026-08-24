// lib/users.ts — browser-side client for the admin Users & Roles BFF surface (story 8.15).
//
// The browser talks ONLY to the Next.js BFF (/api/admin/users*, ADR-013 single choke point); these
// helpers forward the session cookie upstream where the apiserver's requireAdmin gate is the real
// wall. Every call surfaces the upstream status via ApiError so the screen can render the server's
// truth (e.g. 409 "cannot demote the last active admin", 403 for a non-admin) rather than guessing.

export type GlobalRole = "admin" | "user";
export type ProjectRole = "viewer" | "contributor" | "maintainer";

export const PROJECT_ROLES: ProjectRole[] = [
  "viewer",
  "contributor",
  "maintainer",
];

export interface ConsoleUser {
  id: string;
  username: string;
  principal: string;
  email?: string | null;
  teamId: string;
  globalRole: GlobalRole;
  createdAt: string;
  createdBy?: string | null;
  deactivatedAt?: string | null;
}

export interface ProjectMembership {
  project: string;
  role: ProjectRole;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
  ) {
    super(`upstream ${status}`);
  }
}

async function jsonOrThrow(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!res.ok) throw new ApiError(res.status, text);
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    throw new ApiError(res.status, text);
  }
}

/** List all users (admin surface). Fails with ApiError on a non-2xx (401/403 for a non-admin). */
export async function listUsers(): Promise<ConsoleUser[]> {
  const res = await fetch("/api/admin/users?limit=200", { cache: "no-store" });
  const payload = await jsonOrThrow(res);
  const items = (payload as { items?: unknown }).items;
  return Array.isArray(items) ? (items as ConsoleUser[]) : [];
}

/** Change a user's global role (admin ↔ user). 409 ⇒ last-admin guard (surfaced as ApiError). */
export async function setGlobalRole(
  userId: string,
  globalRole: GlobalRole,
): Promise<ConsoleUser> {
  const res = await fetch(`/api/admin/users/${encodeURIComponent(userId)}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ globalRole }),
    cache: "no-store",
  });
  return (await jsonOrThrow(res)) as ConsoleUser;
}

/** Deactivate (soft-delete) a user; upstream also revokes every live session. 204 ⇒ done. */
export async function deactivateUser(userId: string): Promise<void> {
  const res = await fetch(`/api/admin/users/${encodeURIComponent(userId)}`, {
    method: "DELETE",
    cache: "no-store",
  });
  if (!res.ok) throw new ApiError(res.status, await res.text());
}

/** Create a new user. Returns the created user (201). 409 ⇒ username/email already exists. */
export async function createUser(input: {
  username: string;
  password: string;
  globalRole: GlobalRole;
  email?: string;
}): Promise<ConsoleUser> {
  const res = await fetch("/api/admin/users", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(input),
    cache: "no-store",
  });
  return (await jsonOrThrow(res)) as ConsoleUser;
}

/** List a user's per-Project role grants. */
export async function listMemberships(
  userId: string,
): Promise<ProjectMembership[]> {
  const res = await fetch(
    `/api/admin/users/${encodeURIComponent(userId)}/memberships`,
    { cache: "no-store" },
  );
  const payload = await jsonOrThrow(res);
  const items = (payload as { items?: unknown }).items;
  return Array.isArray(items) ? (items as ProjectMembership[]) : [];
}

/** Grant (or update) a user's role on a Project. */
export async function grantMembership(
  userId: string,
  project: string,
  role: ProjectRole,
): Promise<ProjectMembership> {
  const res = await fetch(
    `/api/admin/users/${encodeURIComponent(userId)}/memberships`,
    {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ project, role }),
      cache: "no-store",
    },
  );
  return (await jsonOrThrow(res)) as ProjectMembership;
}

/** Revoke a user's grant on a Project (idempotent upstream). 204 ⇒ done. */
export async function revokeMembership(
  userId: string,
  project: string,
): Promise<void> {
  const res = await fetch(
    `/api/admin/users/${encodeURIComponent(userId)}/memberships?project=${encodeURIComponent(project)}`,
    { method: "DELETE", cache: "no-store" },
  );
  if (!res.ok) throw new ApiError(res.status, await res.text());
}

/** Human-readable message from an ApiError body (best-effort: JSON {error} → text → status). */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    try {
      const parsed = JSON.parse(err.body) as { error?: string };
      if (parsed.error) return parsed.error;
    } catch {
      /* fall through */
    }
    if (err.status === 401 || err.status === 403) {
      return "You must be a fleet admin to manage users and roles.";
    }
    return err.body || `Request failed (${err.status})`;
  }
  return err instanceof Error ? err.message : "Unexpected error";
}
