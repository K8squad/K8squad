// app/api/projects/[projectId]/discussion/threads/[threadId]/messages/[messageId]/route.ts — BFF
// proxy for soft-retracting a message (Story 10.3 AC4). PATCH-ONLY.
//
//   PATCH — soft-retract a message. Author-or-admin only; there is NO hard-delete route (append-only
//           store, §7.4). The apiserver owns the authz decision: a non-author non-admin gets the 403
//           surfaced honestly, and a cross-tenant/absent message is existence-hiding 404 (never
//           re-mapped to 403, AC5). The BFF relays status VERBATIM and asserts no principal of its own.
//
// GET/POST/PUT/DELETE are structurally absent → 405.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function PATCH(
  req: NextRequest,
  {
    params,
  }: {
    params: Promise<{
      projectId: string;
      threadId: string;
      messageId: string;
    }>;
  },
): Promise<Response> {
  const { projectId: p, threadId: t, messageId: m } = await params;
  const projectId = encodeURIComponent(p);
  const threadId = encodeURIComponent(t);
  const messageId = encodeURIComponent(m);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/messages/${messageId}`,
    "PATCH",
  );
}
