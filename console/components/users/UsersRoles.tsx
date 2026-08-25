"use client";

// components/users/UsersRoles.tsx — the admin Users & Roles screen (story 8.15, ISI-2911).
//
// The fleet-admin surface for identity: list every user, change a user's GLOBAL role (admin ↔ user),
// deactivate an account, mint a new user, and manage a user's PER-PROJECT role grants
// (viewer/contributor/maintainer). Two role axes, deliberately distinct: the global role is
// fleet-wide (0008) and drives the adaptive nav (8.16); per-Project grants (auth.project_membership,
// 15.3) are the route-scoped authority the 15.4 wall enforces.
//
// The browser talks ONLY to the BFF (lib/users.ts → /api/admin/users*, ADR-013); the apiserver's
// requireAdmin gate is the real wall. Every mutation surfaces the server's status honestly — a
// non-admin sees a clear "must be a fleet admin" state (the nav already hides this node for them,
// 8.16, but a hand-typed /users still fails closed upstream), and the last-admin guard (409) shows as
// an inline error rather than a silent no-op. Honest states throughout: loading, error, empty — never
// fabricated rows.

import { useCallback, useEffect, useState } from "react";
import {
  createUser,
  deactivateUser,
  errorMessage,
  grantMembership,
  listMemberships,
  listUsers,
  PROJECT_ROLES,
  revokeMembership,
  setGlobalRole,
  type ConsoleUser,
  type GlobalRole,
  type ProjectMembership,
  type ProjectRole,
} from "@/lib/users";

export function UsersRoles() {
  const [users, setUsers] = useState<ConsoleUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null); // userId currently mutating

  const reload = useCallback(async () => {
    setError(null);
    try {
      setUsers(await listUsers());
    } catch (err) {
      setUsers([]);
      setError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const onRoleChange = useCallback(
    async (u: ConsoleUser, role: GlobalRole) => {
      if (role === u.globalRole) return;
      setBusy(u.id);
      setError(null);
      try {
        await setGlobalRole(u.id, role);
        await reload();
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setBusy(null);
      }
    },
    [reload],
  );

  const onDeactivate = useCallback(
    async (u: ConsoleUser) => {
      setBusy(u.id);
      setError(null);
      try {
        await deactivateUser(u.id);
        await reload();
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setBusy(null);
      }
    },
    [reload],
  );

  return (
    <main className="users-page">
      <header className="users-page__head">
        <h1>Users &amp; Roles</h1>
        <p className="muted">
          Fleet identity administration. Change a user&apos;s global role (admin
          grants fleet-wide authority), manage per-Project role grants
          (viewer&nbsp;&lt;&nbsp;contributor&nbsp;&lt;&nbsp;maintainer), or
          deactivate an account. The apiserver is the sole authority — this
          screen mirrors its decisions.
        </p>
      </header>

      {error && (
        <div className="card users-error" role="alert">
          {error}
        </div>
      )}

      <CreateUser onCreated={reload} onError={setError} />

      {users === null ? (
        <div className="card muted">Loading users…</div>
      ) : users.length === 0 && !error ? (
        <div className="card muted">No users yet.</div>
      ) : (
        <div className="card users-table-wrap">
          <table className="users-table">
            <thead>
              <tr>
                <th scope="col">User</th>
                <th scope="col">Email</th>
                <th scope="col">Global role</th>
                <th scope="col">Status</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <UserRow
                  key={u.id}
                  user={u}
                  busy={busy === u.id}
                  onRoleChange={onRoleChange}
                  onDeactivate={onDeactivate}
                  onError={setError}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}

function UserRow({
  user,
  busy,
  onRoleChange,
  onDeactivate,
  onError,
}: {
  user: ConsoleUser;
  busy: boolean;
  onRoleChange: (u: ConsoleUser, role: GlobalRole) => void;
  onDeactivate: (u: ConsoleUser) => void;
  onError: (msg: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const deactivated = Boolean(user.deactivatedAt);

  return (
    <>
      <tr data-deactivated={deactivated || undefined}>
        <td>
          <button
            type="button"
            className="users-expand"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
            title="Toggle per-Project roles"
          >
            <span aria-hidden="true">{open ? "▾" : "▸"}</span>{" "}
            <strong>{user.username}</strong>
          </button>
          <div className="muted users-principal">{user.principal}</div>
        </td>
        <td>{user.email ?? <span className="muted">—</span>}</td>
        <td>
          <label className="sr-only" htmlFor={`role-${user.id}`}>
            Global role for {user.username}
          </label>
          <select
            id={`role-${user.id}`}
            className="users-select"
            value={user.globalRole}
            disabled={busy || deactivated}
            onChange={(e) =>
              onRoleChange(user, e.target.value as GlobalRole)
            }
          >
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </td>
        <td>
          <span
            className="users-badge"
            data-state={deactivated ? "deactivated" : "active"}
          >
            {deactivated ? "deactivated" : "active"}
          </span>
        </td>
        <td className="users-actions">
          {!deactivated && (
            <button
              type="button"
              className="btn btn--danger"
              disabled={busy}
              onClick={() => onDeactivate(user)}
            >
              Deactivate
            </button>
          )}
        </td>
      </tr>
      {open && (
        <tr className="users-memberships-row">
          <td colSpan={5}>
            <Memberships userId={user.id} onError={onError} />
          </td>
        </tr>
      )}
    </>
  );
}

function Memberships({
  userId,
  onError,
}: {
  userId: string;
  onError: (msg: string) => void;
}) {
  const [rows, setRows] = useState<ProjectMembership[] | null>(null);
  const [project, setProject] = useState("");
  const [role, setRole] = useState<ProjectRole>("viewer");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setRows(await listMemberships(userId));
    } catch (err) {
      setRows([]);
      onError(errorMessage(err));
    }
  }, [userId, onError]);

  useEffect(() => {
    void load();
  }, [load]);

  const onGrant = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      const name = project.trim();
      if (!name) return;
      setBusy(true);
      try {
        await grantMembership(userId, name, role);
        setProject("");
        setRole("viewer");
        await load();
      } catch (err) {
        onError(errorMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [project, role, userId, load, onError],
  );

  const onRevoke = useCallback(
    async (name: string) => {
      setBusy(true);
      try {
        await revokeMembership(userId, name);
        await load();
      } catch (err) {
        onError(errorMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [userId, load, onError],
  );

  return (
    <div className="users-memberships">
      <h2 className="users-memberships__title">Per-Project roles</h2>
      {rows === null ? (
        <p className="muted">Loading grants…</p>
      ) : rows.length === 0 ? (
        <p className="muted">No Project grants. This user is a base user.</p>
      ) : (
        <ul className="users-grants">
          {rows.map((m) => (
            <li key={m.project} className="users-grant">
              <code className="users-grant__project">{m.project}</code>
              <span className="users-badge" data-role={m.role}>
                {m.role}
              </span>
              <button
                type="button"
                className="btn"
                disabled={busy}
                onClick={() => onRevoke(m.project)}
              >
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}

      <form className="users-grant-form" onSubmit={onGrant}>
        <label className="sr-only" htmlFor={`proj-${userId}`}>
          Project name
        </label>
        <input
          id={`proj-${userId}`}
          className="users-input"
          placeholder="project name"
          value={project}
          onChange={(e) => setProject(e.target.value)}
          disabled={busy}
        />
        <label className="sr-only" htmlFor={`grantrole-${userId}`}>
          Role
        </label>
        <select
          id={`grantrole-${userId}`}
          className="users-select"
          value={role}
          onChange={(e) => setRole(e.target.value as ProjectRole)}
          disabled={busy}
        >
          {PROJECT_ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <button
          type="submit"
          className="btn btn--primary"
          disabled={busy || project.trim() === ""}
        >
          Grant
        </button>
      </form>
    </div>
  );
}

function CreateUser({
  onCreated,
  onError,
}: {
  onCreated: () => void;
  onError: (msg: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [globalRole, setRole] = useState<GlobalRole>("user");
  const [busy, setBusy] = useState(false);

  const reset = () => {
    setUsername("");
    setPassword("");
    setEmail("");
    setRole("user");
  };

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await createUser({
        username: username.trim(),
        password,
        globalRole,
        email: email.trim() || undefined,
      });
      reset();
      setOpen(false);
      onCreated();
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <div className="users-create-toggle">
        <button
          type="button"
          className="btn btn--primary"
          onClick={() => setOpen(true)}
        >
          + New user
        </button>
      </div>
    );
  }

  return (
    <form className="card users-create" onSubmit={onSubmit}>
      <h2 className="users-create__title">New user</h2>
      <div className="users-create__grid">
        <label>
          Username
          <input
            className="users-input"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
            disabled={busy}
          />
        </label>
        <label>
          Password
          <input
            className="users-input"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            minLength={8}
            required
            disabled={busy}
          />
        </label>
        <label>
          Email (optional)
          <input
            className="users-input"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            disabled={busy}
          />
        </label>
        <label>
          Global role
          <select
            className="users-select"
            value={globalRole}
            onChange={(e) => setRole(e.target.value as GlobalRole)}
            disabled={busy}
          >
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </label>
      </div>
      <div className="users-create__actions">
        <button
          type="button"
          className="btn"
          onClick={() => {
            reset();
            setOpen(false);
          }}
          disabled={busy}
        >
          Cancel
        </button>
        <button
          type="submit"
          className="btn btn--primary"
          disabled={busy || username.trim() === "" || password.length < 8}
        >
          Create user
        </button>
      </div>
    </form>
  );
}
