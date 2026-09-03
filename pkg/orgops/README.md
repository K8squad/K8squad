# orgops — run-scoped board-ops coord API (ISI-3626, ADR-0005)

The privileged sibling of the task-io seam (ISI-3601). A running agent drives it
with the **same** `KSQUAD_COORD_TOKEN` it uses for task-io, POSTing to
`$KSQUAD_COORD_URL/api/org-ops/<verb>`. Every verb is gated **server-side** on a
**role-derived scope** stamped into that token by the operator at mint time —
never on the skill body or its attachment (ADR-0005 D2).

## Verbs, scopes, and shapes

| Verb (`POST /api/org-ops/…`) | Required scope | Request body |
|------------------------------|----------------|--------------|
| `create-agent`   | `org:write`     | `{"name","runtimeRef":{"name"},"roleRef":{"name"},"model","credentialSecretRef":{"name"},"skillRefs":[…],"modelEndpointRef":{…}?}` |
| `create-skill`   | `org:write`     | `{"name","source":{"type":"inline"\|"git","inline"?,"git":{"repoRef","ref","path"?}?},"permissions":[…]}` |
| `create-project` | `project:write` | `{"name","repo":{"url","ref"?},"goals":[…]}` |
| `archive-project`| `project:write` | `{"name"}` |

All are `POST`. Auth is `Authorization: Bearer $KSQUAD_COORD_TOKEN`. Forward the
Run's `traceparent` so each call joins the Run trace (`orgops.<op>` span).

## Scope derivation (who gets what)

Scope is a property of the run's **Role**, computed from the `ksquad.io/reports-to`
Role graph — so per-Agent `skillRefs` cannot widen it (the union loophole,
ADR-0005 D2):

- **`org:write`** — CEO **and** manager roles (any Role that is a
  `ksquad.io/reports-to` target).
- **`project:write`** — the CEO role only (a manager that reports to no one — the
  hierarchy root).
- **neither** — IC (leaf) roles. task-io still works; every org/project verb
  returns `403`.

## Status codes

`201` created · `200` archived · `400` bad body / missing name · `401` missing /
bad / expired token · `403` token lacks the required scope · `404` no
Project to archive **or** the run's namespace is unresolvable · `409` object
already exists · `422` field validation / admission rejection.

## Tenancy

Every write lands in the **calling Run's own namespace** (resolved from the
token's `RUN_ID`), so `org:write` cannot reach across squads.

## Notes / follow-ups

- `archive-project` stamps `ksquad.io/archived=true` + `ksquad.io/archived-at`
  (annotation) — the Project CRD has no first-class lifecycle field yet. A
  `Project.spec` lifecycle field is a follow-up.
- The seam mounts only when the apiserver has **both** a ≥32-byte JWT signing key
  (to verify the run token) and a CRD write client. Otherwise it is absent.
