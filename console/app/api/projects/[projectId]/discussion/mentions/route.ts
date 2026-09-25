// app/api/projects/[projectId]/discussion/mentions/route.ts — BFF proxy for the
// discussion @-mention search (ISI-4926; wired into the composer by ISI-4929).
//
//   GET — agent + work-item suggestions for the composer's `@` popover, scoped
//         server-side to the caller's Team (the apiserver derives the scope from
//         the server-stamped AuthorContext, never from the request). The `q`
//         search text is forwarded VERBATIM; the apiserver owns the
//         deny-by-default authz gate and its response surfaces here verbatim —
//         a deny is existence-hiding (404, never re-mapped to 403).
//
// POST et al. are structurally absent → 405.

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string }> },
): Promise<Response> {
  const projectId = encodeProjectId((await params).projectId);
  // Forward the caller's ?q= verbatim (the endpoint requires a non-empty q).
  const search = req.nextUrl.search; // includes leading '?' or ''
  return proxyJson(
    req,
    `/api/projects/${projectId}/discussion/mentions${search}`,
  );
}
