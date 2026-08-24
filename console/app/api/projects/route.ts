// app/api/projects/route.ts — BFF route feeding the Project context selector (story 8.13).
//
// Maps the apiserver's squad-overview read model (GET /api/squad/overview, story 8.1) into the
// flat project list the selector needs. GET-only; no mutating verb is routed here (405s fall
// through from the route handler by omission). If the upstream read model is not wired yet the
// BFF surfaces its status verbatim and the selector degrades to its empty state — the URL
// remains the active-project source of truth either way.

import type { NextRequest } from "next/server";
import { apiserverBaseUrl } from "@/lib/bff";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";
export const fetchCache = "force-no-store";

type OverviewWire = {
  projects?: { name: string; namespace?: string }[];
};

export async function GET(req: NextRequest): Promise<Response> {
  const upstream = await fetch(`${apiserverBaseUrl()}/api/squad/overview`, {
    method: "GET",
    headers: {
      accept: "application/json",
      ...(req.headers.get("cookie")
        ? { cookie: req.headers.get("cookie") as string }
        : {}),
    },
    cache: "no-store",
    signal: req.signal,
  });
  if (!upstream.ok) {
    return new Response(JSON.stringify({ projects: [] }), {
      status: 200,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  }
  const body = (await upstream.json()) as OverviewWire;
  const projects = (body.projects ?? []).map((p) => ({
    id: p.namespace ? `${p.namespace}/${p.name}` : p.name,
    name: p.name,
  }));
  return new Response(JSON.stringify({ projects }), {
    status: 200,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}
