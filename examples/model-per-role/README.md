# Model-Per-Role examples

Worked example CRs for [Model-Per-Role](../../docs/model-per-role.md)
(ISI-4430) — choosing *where* a Run's model is decided: on the Agent, on a
shared Role, or as a single system default. Resolution walks
**agent → role → default** and takes the first tier that sets a model, as a
unit (that tier's fallback and endpoint come with it).

| File | Shows |
| --- | --- |
| `00-modelconfig-default.yaml` | The **system-default** tier — the required floor (`ModelConfig` named `default` in `k8squad-system`), with an optional fallback and BYO endpoint. |
| `01-role-scoped-model.yaml` | A **role-scoped** model — `Role.spec.model` set once for a whole squad. |
| `02-agent-role-scoped.yaml` | An **Agent that inherits** its model from the Role — `spec.model` omitted, so it resolves to the role tier. |

## Try it

The default `ModelConfig` normally ships from the chart
(`helm ... --set modelConfig.default.model=...`); apply `00-*.yaml` only if you
manage the default tier out-of-band. The role recipe needs a namespace, a
prompt ConfigMap, a runtime, and a credential Secret that this example does not
ship — wire those from the [`codex`](../codex/) or [`bmad-team`](../bmad-team/)
examples, then:

```sh
kubectl create namespace model-per-role-demo
kubectl apply -f 01-role-scoped-model.yaml
kubectl apply -f 02-agent-role-scoped.yaml
```

Agent `coder-1` resolves to the Role's `claude-sonnet-4` (role tier). Change
`Role.spec.model` and every Agent assuming that Role follows.

## Fail-closed

Deleting the system-default `ModelConfig` and clearing both `Agent.spec.model`
and `Role.spec.model` leaves an Agent with no model in any tier — admission
**rejects** it (an undispatchable Agent is never admitted). A merely *missing*
default while the Agent still resolves on its own is a non-blocking admission
**warning**. See [the operator docs](../../docs/model-per-role.md#fail-closed-semantics).
