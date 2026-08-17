# Discussion room — console slice (Story 10.3 / ISI-2704)

Project-scoped **Discussion** route + view: threaded history, author/provenance
badges (agent / human / Run), post + reply-in-thread. A **pure consumer** of the
10.1 apiserver surface behind the shared BFF authorization choke point — a
collaboration surface with **no coordination affordance** (arch §7.5 / §13,
no-P2P applied to the console).

Spec: `docs/bmad/stories/10-3-discussion-room-in-console.md`.
Mock: `docs/bmad/ux/images/07-discussion-room.svg`.

## Layout

| File | Responsibility | AC |
| --- | --- | --- |
| `lib/discussion/types.ts` | API types mirroring `internal/discussion` JSON | — |
| `lib/discussion/provenance.ts` | `deriveAuthorBadge` — agent/human/Run/defect | **AC2** |
| `lib/discussion/compose.ts` | `buildPostBody` — the `{body,parentId?}`-only wire body | **AC3** |
| `lib/discussion/thread.ts` | `nestMessages` — adjacency (`parentId`) → tree | **AC1** |
| `lib/discussion/liveFeed.ts` | SSE reducer — idempotent-by-id live append | **AC6** |
| `lib/discussion/sse.ts` | subscribe over the ONE 8.2 EventSource/BFF proxy | **AC6** |
| `lib/discussion/theme.ts` | `#{BASE}55` theme-invariant chip border tokens | **AC6** |
| `lib/discussion/api.ts` | BFF client; `classifyStatus` 401/403/404→not-found | **AC4** |
| `components/discussion/AuthorBadge.tsx` | badge + Run deep-link (8.11) + defect marker | **AC2** |
| `components/discussion/MessageItem.tsx` | message row + tombstone for retracted | AC1/2 |
| `components/discussion/Composer.tsx` | post/reply; no coordination control | AC3/**AC5** |
| `components/discussion/DiscussionRoom.tsx` | the view: threads + live append + 404 render | AC1/4/6 |
| `app/projects/[projectId]/discussion/page.tsx` | Next.js App Router entry | AC1 |

Tests in `console/test/` — `npm test` (56 cases, all ACs). Every AC crux is a
test: provenance triple exhaustive (incl. the no-fabricated-author defect), the
composer/client never emit an `author_*` field, 401/403/404 collapse to a single
not-found path, the static no-coordination scan, and the theme-invariant border.

## Reconciliation with the landed 10.1 API

The story is written against the Epic-10 10.1 *spec* (`author_agent_id` /
`author_run_id` / `invalidated_at` columns, `…/threads` endpoints). The API that
is **actually landed on `main`** is `internal/discussion` (ISI-2147): it carries
provenance as `authorType` (`agent|human|system`) + `authorName` + a `metadata`
JSONB, with endpoints `…/rooms` and `…/rooms/{roomId}/messages` (`parentId`).

This slice consumes the **landed** shape and maps the story's provenance triple
onto it:

- **agent** ⇐ `authorType === "agent"`
- **human** ⇐ `authorType === "human"`
- **Run chip** ⇐ `metadata.runId` (also accepts `metadata.author_run_id`) →
  deep-links to the Run detail page (Story 8.11)
- **retracted tombstone** ⇐ `metadata.retracted` / `metadata.invalidatedAt`
  (forward-compatible with the spec's `invalidated_at`)

## Known dependency gap (follow-up)

The landed `internal/discussion` **write** handler (`postMessage`) decodes author
fields **from the request body** and does not stamp them from
`PrincipalFromContext` (`auth.go`). AC3's end-to-end guarantee — provenance is
server-stamped, the console sends no `author` — therefore needs the 10.1 handler
to ignore body `author_*` and stamp from the authenticated principal. This
console already sends only `{body, parentId?}` (verified by test); the server
half is tracked as a 10.1 follow-up.

> Scope note: the broader Epic 8 shell (nav rail, auth session, BFF middleware,
> other routes) is provided by Epic 8. This slice adds the Discussion route and
> the seams it needs (BFF client, SSE subscription); it also bootstraps the
> console npm package (Next.js was not yet scaffolded in-repo).
