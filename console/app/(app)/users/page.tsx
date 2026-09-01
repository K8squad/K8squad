// app/users/page.tsx — the Users & Roles nav node (stories 8.15 + 8.16, ISI-2911).
//
// The admin identity surface inside the Epic 8 shell. This route is the thin App Router entry; the
// screen itself (list/create/role-change/deactivate + per-Project grants) is a client component that
// talks to the BFF. RBAC is enforced upstream by the apiserver's requireAdmin gate (§13 choke
// point) — the adaptive nav (8.16) already hides this node for a non-admin, and a hand-typed /users
// still fails closed to the "must be a fleet admin" state because the BFF relays the 403 verbatim.

import { UsersRoles } from "@/components/users/UsersRoles";

export const dynamic = "force-dynamic";

export default function UsersPage() {
  return <UsersRoles />;
}
