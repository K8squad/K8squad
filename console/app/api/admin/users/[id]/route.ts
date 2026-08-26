// app/api/admin/users/[id]/route.ts — BFF proxy for a single user's role change + deactivation
// (story 8.15). PATCH changes the global role (admin ↔ user) / email; DELETE deactivates the user
// (and upstream revokes every live session). Both ride requireAdmin + the same-origin guard on the
// apiserver; status (incl. 409 "cannot demote/deactivate the last active admin") is relayed VERBATIM
// so the screen surfaces the server's truth rather than guessing.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function PATCH(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const id = encodeURIComponent((await params).id);
  return proxyJsonWrite(req, `/admin/users/${id}`, "PATCH");
}

export async function DELETE(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const id = encodeURIComponent((await params).id);
  return proxyJsonWrite(req, `/admin/users/${id}`, "DELETE");
}
