// app/api/projects/[projectId]/discussion/threads/[threadId]/messages/route.ts — BFF proxy for a
// thread's messages (Story 10.3 AC3). POST-ONLY.
//
//   POST — post a message or reply-in-thread. The console body is ONLY `{ body, parentId? }`;
//          provenance (author_*) is stamped SERVER-SIDE from the authenticated principal
//          (internal/discussion/handler.go). The BFF relays the body UNCHANGED and asserts no
//          principal of its own (AC3).
//
// The write traverses the ONE authz choke point (arch §13 / ADR-013): the apiserver owns the
// deny-by-default gate and this route surfaces its status VERBATIM — a deny is existence-hiding
// (404 never re-mapped to 403, AC5). GET history comes from the parent thread route; PUT/DELETE → 405.

import type { NextRequest } from "next/server";
import { proxyJsonWrite } from "@/lib/bff";
import { encodeProjectId } from "@/lib/projectId";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ projectId: string; threadId: string }> },
): Promise<Response> {
  const { projectId: p, threadId: t } = await params;
  const projectId = encodeProjectId(p);
  const threadId = encodeURIComponent(t);
  return proxyJsonWrite(
    req,
    `/api/projects/${projectId}/discussion/threads/${threadId}/messages`,
    "POST",
  );
}
