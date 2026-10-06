// DecisionCard — the decision-request card for a kind='decision_request' message
// (ISI-5536 / ISI-5531 E2, ADR-0026 §4; mock ux/isi-5531-inbox/03-decision-card).
//
// The net-new "an agent suggests structured options a human answers" surface. It
// renders all four modes — approve / choose_one / choose_many / free_form — with a
// free-text escape (Paperclip's gap), a recommended pre-select, and reject-with-
// reason. Like ProposalCard it is a PURE consumer: it never decides anything, it
// renders what the server gives it and hands the typed answer / rejection back to
// the container, which POSTs the answer|reject shell. Once the card leaves `open`
// it renders its decided state read-only (the audit trail lives in the thread).
//
// a11y (mock card contract): the choose_* options are a radiogroup / checkbox group;
// number keys 1–9 pick an option; Enter submits.

import { useCallback, useMemo, useState } from "react";
import type {
  DecisionMode,
  DecisionRequest,
  DecisionRequestPayload,
} from "@/lib/discussion/types";

/** The answer a human submits — the exact body the answer shell accepts (mode is server-derived). */
export interface DecisionAnswerInput {
  selectedOptionIds?: string[];
  freeText?: string;
}

/** Chip label + hue per mode (ADR-0026 §3.4: approve→green, choose_*→blue, free_form→amber). */
export function decisionModeChip(mode: DecisionMode): { label: string; hue: string } {
  switch (mode) {
    case "approve":
      return { label: "Approve", hue: "green" };
    case "choose_one":
      return { label: "Choose option", hue: "blue" };
    case "choose_many":
      return { label: "Choose options", hue: "blue" };
    case "free_form":
      return { label: "Open answer", hue: "amber" };
    default:
      return { label: "Decision", hue: "blue" };
  }
}

/** The free-text escape is offered whenever the card allows it, and is implied for free_form. */
function freeTextOffered(p: DecisionRequestPayload): boolean {
  return p.mode === "free_form" || Boolean(p.allowFreeText);
}

/** The option ids pre-selected on open: explicit defaults, else the recommended option(s). */
function initialSelection(p: DecisionRequestPayload): string[] {
  if (p.defaultSelectedOptionIds && p.defaultSelectedOptionIds.length > 0) {
    return [...p.defaultSelectedOptionIds];
  }
  const recommended = (p.options ?? []).filter((o) => o.recommended).map((o) => o.id);
  if (p.mode === "choose_one") return recommended.slice(0, 1);
  return recommended;
}

export interface DecisionCardProps {
  /** The decision card joined with its lifecycle row (list-decision-requests). */
  decision: DecisionRequest;
  /** Human answer verb — the container POSTs the answer shell (ADR-0026 §4.4). */
  onAnswer?: (messageId: string, answer: DecisionAnswerInput) => void;
  /** Human reject verb — the container POSTs the reject shell; reason required when the card says so. */
  onReject?: (messageId: string, reason?: string) => void;
  /** True while an answer/reject round-trip is in flight. */
  busy?: boolean;
}

export function DecisionCard({ decision, onAnswer, onReject, busy }: DecisionCardProps) {
  const payload = decision.Payload;
  const { mode } = payload;
  const open = decision.phase === "open";
  const chip = decisionModeChip(mode);
  const options = payload.options ?? [];
  const offersFreeText = freeTextOffered(payload);

  const [selected, setSelected] = useState<string[]>(() => initialSelection(payload));
  const [freeTextOn, setFreeTextOn] = useState(false);
  const [freeText, setFreeText] = useState("");
  const [rejecting, setRejecting] = useState(false);
  const [rejectReason, setRejectReason] = useState("");

  const toggleOne = useCallback((id: string) => {
    setFreeTextOn(false);
    setSelected([id]);
  }, []);

  const toggleMany = useCallback((id: string) => {
    setSelected((cur) =>
      cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id],
    );
  }, []);

  // Number keys 1–9 pick the Nth option (mock card contract); Enter submits.
  const onOptionsKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      const n = Number(e.key);
      if (n >= 1 && n <= 9 && n <= options.length) {
        const id = options[n - 1].id;
        mode === "choose_many" ? toggleMany(id) : toggleOne(id);
        e.preventDefault();
      }
    },
    [options, mode, toggleOne, toggleMany],
  );

  // The answer is submittable once the selection satisfies the card's mode contract.
  const freeTextActive = offersFreeText && (mode === "free_form" || freeTextOn);
  const canSubmit = useMemo(() => {
    if (busy) return false;
    if (freeTextActive) return freeText.trim().length > 0;
    switch (mode) {
      case "approve":
        return true;
      case "choose_one":
        return selected.length === 1;
      case "choose_many": {
        const min = payload.minSelected ?? 1;
        const max = payload.maxSelected ?? options.length;
        return selected.length >= min && selected.length <= max;
      }
      case "free_form":
        return freeText.trim().length > 0;
      default:
        return false;
    }
  }, [busy, freeTextActive, freeText, mode, selected, payload, options.length]);

  const submitAnswer = useCallback(() => {
    if (!canSubmit) return;
    if (freeTextActive) {
      onAnswer?.(decision.Message.id, { freeText: freeText.trim() });
      return;
    }
    if (mode === "approve") {
      onAnswer?.(decision.Message.id, {});
      return;
    }
    onAnswer?.(decision.Message.id, { selectedOptionIds: selected });
  }, [canSubmit, freeTextActive, freeText, mode, selected, onAnswer, decision.Message.id]);

  const submitReject = useCallback(() => {
    const reason = rejectReason.trim();
    if (payload.rejectRequiresReason && reason.length === 0) return; // handler also enforces (400)
    onReject?.(decision.Message.id, reason || undefined);
  }, [rejectReason, payload.rejectRequiresReason, onReject, decision.Message.id]);

  const onFormKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Enter" && !rejecting && (e.target as HTMLElement).tagName !== "TEXTAREA") {
        submitAnswer();
        e.preventDefault();
      }
    },
    [rejecting, submitAnswer],
  );

  return (
    <div
      className="ksq-decision"
      data-testid="decision-card"
      data-mode={mode}
      data-phase={decision.phase}
      data-hue={chip.hue}
    >
      <div className="ksq-decision__head">
        <span
          className="ksq-chip ksq-decision__mode"
          data-testid="decision-mode-chip"
          data-hue={chip.hue}
        >
          {chip.label}
        </span>
      </div>

      <h3 className="ksq-decision__title" data-testid="decision-title">
        {payload.title}
      </h3>
      {payload.detailsMarkdown ? (
        <p className="ksq-decision__details">{payload.detailsMarkdown}</p>
      ) : null}

      {/* Decided (read-only) — the card left `open`; the answer lives in the thread. */}
      {!open ? (
        <div className="ksq-decision__decided" data-testid="decision-decided" data-phase={decision.phase}>
          <span className="ksq-chip ksq-decision__phase" data-phase={decision.phase}>
            {decision.phase}
          </span>
          {decision.answeredBy ? <span className="ksq-decision__by"> by {decision.answeredBy}</span> : null}
          {decision.rejectReason ? (
            <p className="ksq-decision__reason" data-testid="decision-reject-reason">
              {decision.rejectReason}
            </p>
          ) : null}
        </div>
      ) : (
        <form
          className="ksq-decision__form"
          data-testid="decision-form"
          onKeyDown={onFormKeyDown}
          onSubmit={(e) => {
            e.preventDefault();
            submitAnswer();
          }}
        >
          {/* choose_one / choose_many — the option group */}
          {(mode === "choose_one" || mode === "choose_many") && (
            <div
              className="ksq-decision__options"
              data-testid="decision-options"
              role={mode === "choose_one" ? "radiogroup" : "group"}
              aria-label={payload.title}
              tabIndex={0}
              onKeyDown={onOptionsKeyDown}
            >
              {options.map((o, i) => {
                const checked = !freeTextOn && selected.includes(o.id);
                return (
                  <label
                    key={o.id}
                    className="ksq-decision__option"
                    data-testid="decision-option"
                    data-checked={checked}
                  >
                    <input
                      type={mode === "choose_one" ? "radio" : "checkbox"}
                      name={`decision-${decision.Message.id}`}
                      checked={checked}
                      disabled={busy}
                      onChange={() => (mode === "choose_many" ? toggleMany(o.id) : toggleOne(o.id))}
                    />
                    <span className="ksq-decision__option-label">{o.label}</span>
                    {o.recommended ? (
                      <span className="ksq-chip ksq-decision__recommended" data-testid="decision-recommended">
                        Recommended
                      </span>
                    ) : null}
                    {o.description ? (
                      <span className="ksq-decision__option-desc">{o.description}</span>
                    ) : null}
                    <span className="ksq-decision__option-key" aria-hidden="true">
                      {i + 1}
                    </span>
                  </label>
                );
              })}

              {/* "Something else — tell me what" free-text escape (Paperclip's gap). */}
              {offersFreeText ? (
                <label className="ksq-decision__option ksq-decision__option--free" data-checked={freeTextOn}>
                  <input
                    type={mode === "choose_one" ? "radio" : "checkbox"}
                    name={`decision-${decision.Message.id}`}
                    checked={freeTextOn}
                    disabled={busy}
                    onChange={() => {
                      setFreeTextOn((v) => !v);
                      if (mode === "choose_one") setSelected([]);
                    }}
                    data-testid="decision-freetext-toggle"
                  />
                  <span className="ksq-decision__option-label">
                    {payload.freeTextLabel || "Something else — tell me what"}
                  </span>
                </label>
              ) : null}
            </div>
          )}

          {/* free_form, or the activated free-text escape — the text answer. */}
          {freeTextActive ? (
            <textarea
              className="ksq-decision__freetext"
              data-testid="decision-freetext"
              value={freeText}
              disabled={busy}
              placeholder={payload.freeTextLabel || "Type your answer…"}
              onChange={(e) => setFreeText(e.target.value)}
            />
          ) : null}

          {/* approve = typed yes/no: Approve submits, Reject opens the reason path. */}
          {mode === "approve" ? (
            <div className="ksq-decision__approve">
              <button
                type="submit"
                className="ksq-btn ksq-btn--approve"
                data-testid="decision-approve"
                disabled={!canSubmit}
              >
                Approve
              </button>
              <button
                type="button"
                className="ksq-btn ksq-btn--reject"
                data-testid="decision-reject-open"
                disabled={busy}
                onClick={() => setRejecting(true)}
              >
                Reject
              </button>
            </div>
          ) : (
            <div className="ksq-decision__actions">
              {payload.allowReject !== false ? (
                <button
                  type="button"
                  className="ksq-link ksq-decision__reject-link"
                  data-testid="decision-reject-open"
                  disabled={busy}
                  onClick={() => setRejecting(true)}
                >
                  Reject with reason
                </button>
              ) : (
                <span />
              )}
              <button
                type="submit"
                className="ksq-btn ksq-btn--primary"
                data-testid="decision-submit"
                disabled={!canSubmit}
              >
                Submit answer
              </button>
            </div>
          )}

          {/* Reject-with-reason panel (ADR-0026 §4.4; reason required when the card says so). */}
          {rejecting ? (
            <div className="ksq-decision__reject" data-testid="decision-reject-panel">
              <textarea
                className="ksq-decision__reject-input"
                data-testid="decision-reject-reason-input"
                value={rejectReason}
                disabled={busy}
                placeholder={
                  payload.rejectRequiresReason ? "A reason is required to reject" : "Reason (optional)"
                }
                onChange={(e) => setRejectReason(e.target.value)}
              />
              <div className="ksq-decision__reject-actions">
                <button
                  type="button"
                  className="ksq-btn"
                  data-testid="decision-reject-cancel"
                  disabled={busy}
                  onClick={() => setRejecting(false)}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  className="ksq-btn ksq-btn--reject"
                  data-testid="decision-reject-confirm"
                  disabled={busy || (payload.rejectRequiresReason && rejectReason.trim().length === 0)}
                  onClick={submitReject}
                >
                  Confirm reject
                </button>
              </div>
            </div>
          ) : null}
        </form>
      )}
    </div>
  );
}

export default DecisionCard;
