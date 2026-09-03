"use client";

import { ComposeForm, FieldErrors } from "@/lib/compose";
import { Field } from "./fields";

interface AgentFormProps {
  cf: ComposeForm & { kind: "agents" };
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function AgentForm({ cf, errors, patch }: AgentFormProps) {
  const nameField = (
    <Field label="Name" hint="DNS-1123 label (lowercase, digits, '-')" error={errors["name"]}>
      <input
        value={cf.form.name}
        onChange={(e) => patch({ name: e.target.value })}
        aria-invalid={!!errors["name"]}
        placeholder="my-resource"
      />
    </Field>
  );

  return (
    <div className="compose__grid">
      <Field label="Project" hint="the squad this Agent composes within" error={errors["project"]}>
        <input
          value={cf.form.project}
          onChange={(e) => patch({ project: e.target.value })}
          aria-invalid={!!errors["project"]}
        />
      </Field>
      {nameField}
      <Field label="Runtime ref" error={errors["runtimeRef.name"]}>
        <input
          value={cf.form.runtimeRef}
          onChange={(e) => patch({ runtimeRef: e.target.value })}
          aria-invalid={!!errors["runtimeRef.name"]}
          placeholder="name or namespace/name"
        />
      </Field>
      <Field label="Role ref" error={errors["roleRef.name"]}>
        <input
          value={cf.form.roleRef}
          onChange={(e) => patch({ roleRef: e.target.value })}
          aria-invalid={!!errors["roleRef.name"]}
        />
      </Field>
      <Field label="Model" error={errors["model"]}>
        <input
          value={cf.form.model}
          onChange={(e) => patch({ model: e.target.value })}
          aria-invalid={!!errors["model"]}
          placeholder="claude-opus-4-8"
        />
      </Field>
      <Field label="Credential Secret ref" hint="name or name/key" error={errors["credentialSecretRef.name"]}>
        <input
          value={cf.form.credentialSecretRef}
          onChange={(e) => patch({ credentialSecretRef: e.target.value })}
          aria-invalid={!!errors["credentialSecretRef.name"]}
        />
      </Field>
      <Field label="Model endpoint Secret ref" hint="optional — name or name/key">
        <input
          value={cf.form.modelEndpointRef}
          onChange={(e) => patch({ modelEndpointRef: e.target.value })}
        />
      </Field>
      <Field label="Skill refs" hint="optional — one name (or namespace/name) per line">
        <textarea
          rows={3}
          value={cf.form.skillRefs}
          onChange={(e) => patch({ skillRefs: e.target.value })}
        />
      </Field>
    </div>
  );
}