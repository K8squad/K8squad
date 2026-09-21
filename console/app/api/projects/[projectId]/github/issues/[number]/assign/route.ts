// app/api/projects/[projectId]/github/issues/[number]/assign/route.ts — BFF seam
// for the GitHub-issue → assign-&-dispatch bridge (ISI-4759 / ISI-4749 epic 3,
// Epic-2 contract ISI-4757).
//
// POST-only. The Go apiserver finds-or-creates the work-item for this GitHub issue
// and dispatches the human's chosen agent in ONE atomic call (idempotent on the
// dedicated label `ksquad.github.issue=owner/repo#N`). The BFF relays the caller's
// session and the apiserver's answer VERBATIM — 200 (bridged + dispatched), 400
// (invalid/missing agent), 403 (human-only / agent not in Team), 404 (issue not
// resolvable), 409 (already dispatched / bridged), 501 (bridge seam not hosted).
//
// Honesty (ADR-0013): this is a Paperclip-side dispatch only; it never writes
// GitHub assignees. `encodeProjectId` guards the project segment (ISI-4662
// double-encode fix). CSRF posture matches work-items/[id]/dispatch: proxyJsonWrite
// carries the session cookie on the state-changing POST.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  ctx: { params: Promise<{ projectId: string; number: string }> },
): Promise<Response> {
  const { projectId, number } = await ctx.params;
  return proxyJsonWrite(
    req,
    `/api/projects/${encodeProjectId(projectId)}/github/issues/${encodeURIComponent(number)}/assign`,
    "POST",
  );
}
