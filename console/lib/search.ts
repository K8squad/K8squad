// lib/search.ts — shared types + pure helpers for the top-bar global search (story 8.19 / ISI-2912).
//
// The data contract mirrors the apiserver's GET /api/search projection behind the BFF choke point
// (§13 / ADR-013): the browser calls the Next.js route `/api/search?q=&limit=`, which forwards the
// query string verbatim and relays the apiserver's status (a 400 on blank q, a deny 401/403/404,
// an upstream 5xx) unchanged. Presentation helpers are PURE so the component stays a thin render and
// they are unit-testable (Vitest) without a DOM.
//
// Honesty rule carried from the house pattern: the server-generated `snippet` is the ONLY source of
// highlight markup and it contains ONLY <mark>…</mark> tags (Postgres ts_headline StartSel/StopSel).
// It is rendered via innerHTML, so it MUST be sanitized here — escape ALL html, then re-admit ONLY
// the literal <mark>/</mark> pair — so an injected <img onerror> in a title/snippet can never execute.

/** One search hit (mirror of the apiserver's SearchResult). `type` is "work_item" today; the shape
 *  is forward-compatible for more result kinds later. */
export interface SearchResult {
  type: string;
  id: string;
  /** Owning Project id, or "" when the hit is not Project-scoped. */
  projectId: string;
  title: string;
  /** Server-generated excerpt carrying ONLY <mark>…</mark> highlight tags — sanitize before render. */
  snippet: string;
  state: string;
  rank: number;
  updatedAt: string;
}

/** GET /api/search page (mirror of the apiserver's SearchResponse): the echoed query + ranked hits. */
export interface SearchResponse {
  query: string;
  results: SearchResult[];
}

/** A non-2xx from the search read model. Carries the upstream status so the UI can surface a
 *  discreet inline error without crashing (a blank-q 400 never reaches here — it is guarded below). */
export class SearchError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`search failed: ${status}`);
    this.name = "SearchError";
    this.status = status;
  }
}

/** The default top-bar dropdown page size; the API default is 20 when the param is absent. */
export const SEARCH_DROPDOWN_LIMIT = 8;

/**
 * Fetch a page of search hits through the BFF. A blank/whitespace-only query is guarded CLIENT-SIDE
 * (the apiserver would answer 400) — we return an empty page WITHOUT touching the network, matching
 * the UI rule "blank query shows nothing". `signal` lets the caller abort an in-flight request when a
 * newer keystroke supersedes it. A non-2xx becomes a {@link SearchError} carrying the verbatim status.
 */
export async function fetchSearch(
  q: string,
  limit = SEARCH_DROPDOWN_LIMIT,
  signal?: AbortSignal,
): Promise<SearchResponse> {
  const query = q.trim();
  if (!query) return { query: "", results: [] };

  const params = new URLSearchParams();
  params.set("q", query);
  params.set("limit", String(limit));

  const res = await fetch(`/api/search?${params.toString()}`, {
    cache: "no-store",
    signal,
  });
  if (!res.ok) throw new SearchError(res.status);

  const body = (await res.json()) as Partial<SearchResponse> | null;
  const results: SearchResult[] = Array.isArray(body?.results)
    ? body!.results!.map(normalizeResult)
    : [];
  return {
    query: typeof body?.query === "string" ? body!.query! : query,
    results,
  };
}

/** Coerce an untrusted wire object into a SearchResult, defaulting every field (never NaN/undefined
 *  leaking into render). Purely defensive — the contract is fixed, but the BFF forwards bytes. */
function normalizeResult(raw: unknown): SearchResult {
  const r = (raw ?? {}) as Record<string, unknown>;
  return {
    type: typeof r.type === "string" ? r.type : "work_item",
    id: typeof r.id === "string" ? r.id : "",
    projectId: typeof r.projectId === "string" ? r.projectId : "",
    title: typeof r.title === "string" ? r.title : "",
    snippet: typeof r.snippet === "string" ? r.snippet : "",
    state: typeof r.state === "string" ? r.state : "",
    rank: typeof r.rank === "number" ? r.rank : 0,
    updatedAt: typeof r.updatedAt === "string" ? r.updatedAt : "",
  };
}

/**
 * Sanitize a server snippet/title for innerHTML rendering: escape EVERY html metacharacter, then
 * re-admit ONLY the literal <mark>/</mark> pair (which ts_headline emits). Anything the server did
 * not intend as a highlight — including an injected `<img onerror=…>` or `<script>` — stays escaped
 * as inert text. This is the ONLY place raw markup is trusted, and it trusts exactly one tag.
 */
export function sanitizeSnippet(raw: string): string {
  return escapeHtml(raw)
    .replace(/&lt;mark&gt;/g, "<mark>")
    .replace(/&lt;\/mark&gt;/g, "</mark>");
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

/** State badge tone: mirrors the work-item lifecycle palette used across the console. */
export type SearchStateBadge = { label: string; tone: "idle" | "info" | "warn" | "ok" };

const STATE_BADGES: Record<string, SearchStateBadge> = {
  backlog: { label: "Backlog", tone: "idle" },
  todo: { label: "To do", tone: "idle" },
  in_progress: { label: "In progress", tone: "info" },
  in_review: { label: "In review", tone: "warn" },
  done: { label: "Done", tone: "ok" },
};

/** Badge for a work-item state; an unknown/empty state renders its raw value (never fabricated). */
export function stateBadge(state: string): SearchStateBadge {
  return STATE_BADGES[state] ?? { label: state || "—", tone: "idle" };
}

/**
 * Where clicking a hit lands. A Project-scoped hit deep-links to that Project's Tickets surface with
 * the item pre-selected (`?item=`); an unscoped hit falls back to the global Overview. The tickets
 * path is the canonical Project → Tickets route (app/projects/[projectId]/tickets).
 */
export function resultHref(r: SearchResult): string {
  if (r.projectId) {
    return `/projects/${encodeURIComponent(r.projectId)}/tickets?item=${encodeURIComponent(r.id)}`;
  }
  return "/overview";
}
