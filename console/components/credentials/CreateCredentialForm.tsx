"use client";

// components/credentials/CreateCredentialForm.tsx — the BYO service-account key paste
// form (ISI-3983) + fleet-admin team picker (ISI-3937), extracted verbatim from
// CredentialsScreen so other surfaces reuse the exact shipped v1 write path instead of
// re-deriving it. First reuse: the Claude branch of RuntimeAdapterStep on Settings → LLM
// (ISI-6702 WS-4) pins fixedRuntime="claude-code" — a user attaches an Anthropic API key
// end-to-end (paste → per-team Secret → the ANTHROPIC_API_KEY test-connection probe),
// while the subscription OAuth CTA stays disabled (ToS-gated, ISI-3661). The value stays
// write-only; create/test stay injectable so the existing screen tests hold unchanged.

import { useCallback, useState, type FormEvent, type ReactNode } from "react";
import {
  classifyCreateStatus,
  credentialCreateBody,
  CREATE_RUNTIME_OPTIONS,
  type CredentialTeamOption,
} from "@/lib/credentials";
import "./credentials.css";

/** Fleet-admin team picker (ISI-3937). Rendered only when the fleet exposes more than one Team;
 *  a single-team caller is auto-selected upstream and never sees it. */
export function TeamPicker({
  teams,
  selected,
  onSelect,
  prompt,
}: {
  teams: CredentialTeamOption[];
  selected: string;
  onSelect: (uid: string) => void;
  prompt: boolean;
}) {
  return (
    <div className="creds__team" data-testid="creds-team-picker">
      <label className="creds__team-label" htmlFor="creds-team-select">
        Team
      </label>
      <select
        id="creds-team-select"
        className="creds__team-select"
        value={selected}
        onChange={(e) => onSelect(e.target.value)}
        data-testid="creds-team-select"
      >
        <option value="">Select a team…</option>
        {teams.map((t) => (
          <option key={t.uid} value={t.uid}>
            {t.name} ({t.namespace})
          </option>
        ))}
      </select>
      {prompt && selected === "" && (
        <p className="muted" data-testid="creds-team-prompt">
          You administer more than one squad — pick a team to view and add its
          credentials.
        </p>
      )}
    </div>
  );
}

/** The BYO service-account paste form (ISI-3983). Value is write-only; the apiserver stores it and
 *  returns a secretRef, never the material. Human-seat OAuth stays the Connect Claude path. */
export function CreateCredentialForm({
  create = defaultCreate,
  test = defaultTest,
  teamId,
  teamReady,
  onCreated,
  fixedRuntime,
  title,
  lede,
}: {
  create?: (body: Record<string, string>) => Promise<Response>;
  test?: (
    name: string,
    body: { runtime: string; teamId?: string },
  ) => Promise<Response>;
  teamId: string;
  teamReady: boolean;
  onCreated?: () => void;
  /**
   * Pin the runtime to one CREATE_RUNTIME_OPTIONS value (e.g. "claude-code" for
   * the Claude lane, ISI-6702): the dropdown locks to it. The write and probe
   * contracts are unchanged.
   */
  fixedRuntime?: string;
  /** Heading override for context-specific slots (default: "Add a credential"). */
  title?: string;
  /** Lede override for context-specific slots (default: the Credentials copy). */
  lede?: ReactNode;
}) {
  const [name, setName] = useState("");
  const [runtime, setRuntime] = useState(
    fixedRuntime ?? CREATE_RUNTIME_OPTIONS[0].value,
  );
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ tone: "ok" | "bad"; text: string } | null>(
    null,
  );
  const [created, setCreated] = useState<{
    name: string;
    runtime: string;
  } | null>(null);
  const [testMsg, setTestMsg] = useState<{
    tone: "ok" | "bad";
    text: string;
  } | null>(null);
  const [testBusy, setTestBusy] = useState(false);

  const onSubmit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      setMsg(null);
      setTestMsg(null);
      setCreated(null);
      if (!name.trim() || !value) {
        setMsg({ tone: "bad", text: "Name and key are both required." });
        return;
      }
      setBusy(true);
      try {
        const res = await create(
          credentialCreateBody({ name, runtime, value, teamId }),
        );
        const outcome = classifyCreateStatus(res.status);
        if (outcome === "created") {
          const body = await res.json().catch(() => ({}));
          const ref = typeof body?.secretRef === "string" ? body.secretRef : "";
          setMsg({
            tone: "ok",
            text: ref ? `Credential stored — ${ref}` : "Credential stored.",
          });
          setCreated({ name: name.trim(), runtime });
          setValue(""); // never keep the pasted key in state longer than the write
          onCreated?.();
        } else {
          setMsg({ tone: "bad", text: await createErrorText(outcome, res) });
        }
      } catch {
        setMsg({
          tone: "bad",
          text: "The credential store is unreachable — try again.",
        });
      } finally {
        setBusy(false);
      }
    },
    [name, runtime, value, teamId, create, onCreated],
  );

  const onTest = useCallback(async () => {
    if (!created) return;
    setTestBusy(true);
    setTestMsg(null);
    try {
      const res = await test(created.name, {
        runtime: created.runtime,
        teamId: teamId || undefined,
      });
      if (res.status >= 200 && res.status < 300) {
        const body = await res.json().catch(() => ({}));
        const ok = body?.ok === true;
        const detail = typeof body?.detail === "string" ? body.detail : "";
        setTestMsg({
          tone: ok ? "ok" : "bad",
          text: ok
            ? `Connected${detail ? ` — ${detail}` : ""}`
            : detail || "The endpoint declined the credential.",
        });
      } else if (res.status === 501) {
        setTestMsg({
          tone: "bad",
          text: "This credential type can't be tested yet.",
        });
      } else {
        setTestMsg({
          tone: "bad",
          text: "Couldn't reach the test probe — try again.",
        });
      }
    } catch {
      setTestMsg({
        tone: "bad",
        text: "Couldn't reach the test probe — try again.",
      });
    } finally {
      setTestBusy(false);
    }
  }, [created, test, teamId]);

  return (
    <form
      id="creds-add"
      className="creds__form"
      data-testid="creds-create-form"
      onSubmit={onSubmit}
    >
      <h2 className="creds__form-title">{title ?? "Add a credential"}</h2>
      <p className="muted creds__form-lede">
        {lede ?? (
          <>
            Bring your own provider key — the v1 path. KSquad stores it as a
            per-team Kubernetes Secret and never shows it back. Zero-touch OAuth
            (Connect Claude) is pending a board Terms-of-Service decision
            (ISI-3661) — no ETA.
          </>
        )}
      </p>
      <div className="creds__form-grid">
        <label className="creds__field">
          <span>Name</span>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. team-anthropic"
            data-testid="creds-name"
            autoComplete="off"
          />
        </label>
        <label className="creds__field">
          <span>Runtime</span>
          <select
            value={runtime}
            onChange={(e) => setRuntime(e.target.value)}
            data-testid="creds-runtime"
            disabled={!!fixedRuntime}
          >
            {(fixedRuntime
              ? CREATE_RUNTIME_OPTIONS.filter((o) => o.value === fixedRuntime)
              : CREATE_RUNTIME_OPTIONS
            ).map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
        <label className="creds__field creds__field--wide">
          <span>Service-account key</span>
          <input
            type="password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="Paste the provider API key"
            data-testid="creds-value"
            autoComplete="off"
          />
        </label>
      </div>
      <div className="creds__form-actions">
        <button
          type="submit"
          className="creds__form-submit"
          disabled={busy || !teamReady}
          data-testid="creds-submit"
        >
          {busy ? "Saving…" : "Save credential"}
        </button>
        {!teamReady && (
          <span className="muted" data-testid="creds-team-required">
            Pick a team above first.
          </span>
        )}
        {created && (
          <button
            type="button"
            className="creds__form-test"
            onClick={onTest}
            disabled={testBusy}
            data-testid="creds-test"
          >
            {testBusy ? "Testing…" : "Test connection"}
          </button>
        )}
      </div>
      {msg && (
        <p
          className={`creds__form-msg creds__form-msg--${msg.tone}`}
          data-testid="creds-create-msg"
          data-tone={msg.tone}
          role="status"
        >
          {msg.text}
        </p>
      )}
      {testMsg && (
        <p
          className={`creds__form-msg creds__form-msg--${testMsg.tone}`}
          data-testid="creds-test-msg"
          data-tone={testMsg.tone}
          role="status"
        >
          {testMsg.text}
        </p>
      )}
    </form>
  );
}

/** Honest, non-leaking copy for each create failure. Field detail for 422 is the apiserver's own
 *  field/message list (it never echoes the value); everything else stays generic. */
async function createErrorText(
  outcome: ReturnType<typeof classifyCreateStatus>,
  res: Response,
): Promise<string> {
  switch (outcome) {
    case "select-team":
      return "Pick a team for this credential first.";
    case "conflict":
      return "A credential with this name already exists in this team.";
    case "unsupported":
      return "Human-seat OAuth isn't pasted — Connect Claude will provision it (pending a board Terms-of-Service decision, ISI-3661).";
    case "denied":
      return "You don't have access to write credentials for this team.";
    case "invalid": {
      const body = await res.json().catch(() => ({}));
      const fields = Array.isArray(body?.fields) ? body.fields : [];
      if (fields.length) {
        return fields
          .map(
            (f: { field?: string; message?: string }) =>
              `${f.field ?? "field"}: ${f.message ?? "invalid"}`,
          )
          .join("; ");
      }
      return "The credential was rejected — check the name, runtime, and key.";
    }
    default:
      return "The credential store rejected the write — try again.";
  }
}

export async function defaultCreate(
  body: Record<string, string>,
): Promise<Response> {
  return fetch("/api/credentials", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

export async function defaultTest(
  name: string,
  body: { runtime: string; teamId?: string },
): Promise<Response> {
  return fetch(`/api/credentials/${encodeURIComponent(name)}/test`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}
