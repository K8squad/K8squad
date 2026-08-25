// app/api/admin/users/[id]/memberships/route.ts — BFF proxy for a user's per-Project role grants
// (story 8.15, ISI-2911). GET lists the user's auth.project_membership rows; PUT grants/updates a
// {project, role}; DELETE?project= revokes one. All ride requireAdmin + the same-origin guard on the
// apiserver (per-Project role administration is a global-admin power). Status relayed VERBATIM. The
// grant store is the SAME instance the 15.4 enforcement wall reads, so a change here takes effect at
// the choke point immediately.

import type { NextRequest } from "next/server";
import { proxyJson, proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const id = encodeURIComponent((await params).id);
  return proxyJson(req, `/admin/users/${id}/memberships`);
}

export async function PUT(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const id = encodeURIComponent((await params).id);
  return proxyJsonWrite(req, `/admin/users/${id}/memberships`, "PUT");
}

export async function DELETE(
  req: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const id = encodeURIComponent((await params).id);
  // Forward the ?project=<name> selector verbatim to the upstream revoke.
  return proxyJsonWrite(
    req,
    `/admin/users/${id}/memberships${req.nextUrl.search}`,
    "DELETE",
  );
}
