// S4c BFF route — Project File Explorer file content (ISI-3991, ADR-0012 §D2 / S4c §Tasks 1).
//
// GET /api/projects/[projectId]/files/content?path=<file>&offset=<n>&length=<n>
//   → proxies to Go apiserver GET /api/projects/{projectId}/files/content?path=&offset=&length=
//
// Pass-through only: no business logic, no status re-mapping.
// The response body is a JSON envelope with base64-encoded data + metadata (size, contentType,
// offset, length, degraded). The console must NOT attempt UTF-8 decode when contentType="binary".
//
// Browser NEVER talks to the apiserver directly; this BFF is the sole authz choke point (§13).

import type { NextRequest } from "next/server";
import { proxyJson } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

export async function GET(
  req: NextRequest,
  { params }: { params: { projectId: string } },
): Promise<Response> {
  const { projectId } = params;
  const qs = new URLSearchParams();
  const path = req.nextUrl.searchParams.get("path");
  if (path) qs.set("path", path);
  const offset = req.nextUrl.searchParams.get("offset");
  if (offset) qs.set("offset", offset);
  const length = req.nextUrl.searchParams.get("length");
  if (length) qs.set("length", length);

  const upstream = `/api/projects/${projectId}/files/content${qs.size ? `?${qs}` : ""}`;
  return proxyJson(req, upstream);
}
