"use client";

import { ComposeForm, FieldErrors } from "@/lib/compose";
import { Field } from "./fields";

interface ProjectFormProps {
  cf: ComposeForm & { kind: "projects" };
  errors: FieldErrors;
  patch: (p: Record<string, unknown>) => void;
}

export function ProjectForm({ cf, errors, patch }: ProjectFormProps) {
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
      {nameField}
      <Field label="Repo URL" error={errors["repo.url"]}>
        <input
          value={cf.form.repoUrl}
          onChange={(e) => patch({ repoUrl: e.target.value })}
          aria-invalid={!!errors["repo.url"]}
          placeholder="https://github.com/org/repo"
        />
      </Field>
      <Field label="Repo ref" hint="optional — branch / tag / SHA">
        <input
          value={cf.form.repoRef}
          onChange={(e) => patch({ repoRef: e.target.value })}
          placeholder="main"
        />
      </Field>
      <Field label="Egress policy ref" hint="optional — name or namespace/name">
        <input
          value={cf.form.egressPolicyRef}
          onChange={(e) => patch({ egressPolicyRef: e.target.value })}
        />
      </Field>
      <Field label="Goals" hint="one goal per line">
        <textarea
          rows={3}
          value={cf.form.goals}
          onChange={(e) => patch({ goals: e.target.value })}
          placeholder={"Ship the checkout flow\nHarden the payment path"}
        />
      </Field>
    </div>
  );
}