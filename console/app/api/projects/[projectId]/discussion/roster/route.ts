// app/api/projects/[projectId]/discussion/roster/route.ts — BFF proxy for the
// discussion room roster (ISI-5107; the missing BFF hop was ISI-5164's empty-roster
// bug: the apiserver endpoint and the client `getRoster` existed, but this proxy
// route did not, so the browser's fetch fell through to Next's 404 and the client
// degraded every room to "No agents on this team yet").
//
//   GET — the agents dispatchable into THIS project for the right-rail roster and
//         the composer's direct-target selector. Scope is derived server-side from
//         the server-stamped AuthorContext (admin ⇒ the project namespace; everyone
//         else ⇒ their own Team), never from the request. The apiserver owns the
//         deny-by-default authz gate; its response — including a `[]` degrade —
//         surfaces here verbatim. A deny is existence-hiding (404, never 403).
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
  return proxyJson(req, `/api/projects/${projectId}/discussion/roster`);
}
