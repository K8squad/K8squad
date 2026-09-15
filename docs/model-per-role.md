# Model Per Role

Model-Per-Role (ISI-4430) lets you set the model **once, at the tier that owns
the decision** — on an individual Agent, on a Role shared by many Agents, or as
a single system-wide default — instead of stamping a model onto every Agent by
hand. Every Run still resolves to exactly one effective model; you just choose
*where* that choice lives.

This page is the operator reference for how that resolution works and how to
configure each tier. For the install-time required default and the full chart
value table, see the [chart README](../config/helm/README.md#set-the-system-default-model-required-for-install).
Working example CRs live in [`examples/model-per-role/`](../examples/model-per-role/).

## The three tiers

Each Run's model is chosen by walking three tiers from most specific to least,
and taking the first one that sets a model:

| Tier | Source field | When it wins |
| --- | --- | --- |
| **agent** | `Agent.spec.model` | Set on the Agent — highest precedence. |
| **role** | `Role.spec.model` | Agent's `spec.model` is empty; the Agent's Role supplies it. |
| **default** | `ModelConfig.spec.model` (the singleton named `default` in the operator namespace) | Neither the Agent nor its Role set a model. The floor. |

`Agent.spec.model` and `Role.spec.model` are both **optional** — an empty value
falls through to the next tier. The **default tier is the floor**: it has
nothing below it, so its `model` is **required** and the install fails without
it (see [Install-time required default](#install-time-required-default)).

```
Agent.spec.model  ──set?──▶ use it              (tier = agent)
       │ empty
       ▼
Role.spec.model   ──set?──▶ use it              (tier = role)
       │ empty
       ▼
ModelConfig "default" .spec.model ──set?──▶ use it   (tier = default)
       │ empty / missing
       ▼
   FAIL-CLOSED  (no effective model → the Agent is undispatchable)
```

## Tier-as-a-unit

The winning tier is taken **as a unit**. Whichever tier supplies the primary
`model` *also* supplies that tier's `fallbackModel` and (where the tier has one)
its `modelEndpointRef`. A lower tier's fallback or endpoint is **never grafted**
onto a higher tier's primary.

Concretely: if an Agent sets `spec.model` but no `fallbackModel`, it does **not**
inherit the Role's or the default's fallback — the agent tier won, so the agent
tier's (absent) fallback is what you get. If you want a fallback for that Agent,
set it on the Agent. This keeps the resolved `(primary, fallback)` pair
internally consistent — it always comes from one place — instead of stitching a
Frankenstein pair across tiers.

Endpoint/credential resolution follows the **winning tier** too:

- **agent tier** — uses `Agent.spec.modelEndpointRef` (its BYO / Ollama /
  OpenAI-compatible endpoint Secret), or the provider-default endpoint when
  unset.
- **role tier** — uses the provider/operator default endpoint. A role-level
  endpoint ref is deferred in v1 (a role-tier model has no BYO endpoint of its
  own yet); a role-tier `fallbackModel` may still carry its own endpoint Secret.
- **default tier** — uses `ModelConfig.spec.modelEndpointRef` when set,
  otherwise the provider-default endpoint.

Within any winning tier, a `fallbackModel` with no `modelEndpointRef` of its own
rides that **same tier's** primary endpoint (same wire, second model).

## Fail-closed semantics

There is no silent fallback to a paid provider and no empty-model Run. If the
tier walk yields no model in **any** tier — not even the system-default
`ModelConfig` — the Agent is **undispatchable**, and this is enforced at two
points:

- **Admission (webhook).** Writing an Agent that resolves to no model in any
  tier is **rejected** with a message telling you to set `spec.model`, give the
  referenced Role a model, or create the system-default `ModelConfig`. A
  dangling endpoint Secret or malformed endpoint URL on the winning tier is
  rejected the same way.
- **Dispatch (reconciler).** The dispatch path runs the identical tier-as-a-unit
  resolver, so what admission proved is exactly what dispatch assumes. A Run
  never starts without a resolved model.

### Missing-default soft-warn

The system-default `ModelConfig` is a cluster-wide floor that many Agents lean
on. If it is **deleted** after install, an Agent that still resolves fine on its
own (because it sets `spec.model`, or its Role does) is **not** rejected — but
the next write of *any* Agent surfaces an **admission warning** that the
system-default `ModelConfig` singleton is absent:

```
Warning: system-default ModelConfig "default" not found in k8squad-system —
Agents/Roles that rely on the default tier will fail admission until it is restored
```

This is deliberate: a deleted default is a *latent* fail-closed risk for every
Agent that relies on the default tier. Surfacing it loudly on the next Agent
write — rather than silently at some future default-tier Run's dispatch — turns
a lurking cluster-wide outage into a visible warning now. The warning never
turns a healthy Agent write into a denial; rejecting an Agent that genuinely
resolves to no model is the fail-closed guard's job, not this one's.

## Install-time required default

The chart ships exactly one system-default `ModelConfig` named `default` in the
operator namespace, templated from `modelConfig.default.*`. Because the default
tier is the floor with nothing below it, **`modelConfig.default.model` has no
default value and the install aborts without it**:

```sh
$ helm install k8squad config/helm -n k8squad-system
Error: execution error at (k8squad/templates/modelconfig.yaml): modelConfig.default.model is required — set the system default model (see chart README)
```

Set it on install or upgrade:

```sh
helm upgrade --install k8squad config/helm -n k8squad-system \
  --set modelConfig.default.model=claude-sonnet-4
```

The `ModelConfig` **CRD** ships in the separate `config/helm-crds` chart
(ADR-0002 Option B) — install it first so the CRD exists before the CR is
applied. Full chart value reference:
[chart README → Set the system default model](../config/helm/README.md#set-the-system-default-model-required-for-install).

## Recipes

### Role-scoped model (set once for a whole squad)

Set `Role.spec.model` and leave `Agent.spec.model` empty on every Agent that
assumes the role. All of them resolve to the role tier — change the model in one
place and the whole squad follows. See
[`examples/model-per-role/`](../examples/model-per-role/) `01-role-scoped-model.yaml`
+ `02-agent-role-scoped.yaml`.

### Custom system default

Override the shipped default via `modelConfig.default.*` chart values, or apply
your own `ModelConfig` named `default` in the operator namespace with a
`fallbackModel` and/or a BYO `modelEndpointRef`. See
[`examples/model-per-role/`](../examples/model-per-role/) `00-modelconfig-default.yaml`.

### Per-Agent override

Set `Agent.spec.model` to pin one Agent to a specific model regardless of its
Role or the default — the agent tier wins. Add a `fallbackModel` on the same
Agent if you want a rate-limit fallback for it (tier-as-a-unit: the Agent's
fallback, not the Role's or the default's).
