# Board-interaction taxonomy — the shared option contract

- **Status:** Accepted
- **Date:** 2026-11-08
- **Issue:** ISI-7227 (WS-6.5, child of plan ISI-7204 rev 1 `7425e66c`, board-accepted 2026-11-08 18:04 UTC; parent epic ISI-7074)
- **Coordination:** ISI-7075 (proactive suggestions — the suggest-next-steps/suggest-tasks half, C4/ISI-7218)
- **Extends:** ADR-0026 §4.1 (`DecisionOption`), implemented in `internal/discussion/decision.go`

## 1. Why this doc exists

The k8squad inbox is the single surface where agents ask the board (humans) to
decide things. Two interaction kinds now feed it, built by different
workstreams:

- **decision_request** (WS-6 / ISI-7074): an agent hits a decision point and
  offers the board a card of labeled options (approve / choose_one /
  choose_many / free_form).
- **suggest_next_steps** (ISI-7075 C4 / ISI-7218): a coordinator reporting
  blocked/stuck must propose concrete next actions (tickets to create, who to
  assign, what to unblock) instead of just stating status.

If the two kinds shape their choices differently, the inbox renders two
dialects of "pick an option" and the board pays the difference every time.
This doc fixes ONE option shape and ONE set of authoring rules for both, so
every "agent suggests, human chooses" card looks and behaves the same.

**Normative rule:** any current or future board interaction that asks the
board to pick among labeled choices MUST present those choices in the shared
option shape below. Interactions that do not ask the board to pick (status
notes, log-style comments) are out of scope and stay plain messages.

## 2. The shared option shape

Wire shape (JSON), Go (`internal/discussion/decision.go`), and TypeScript
(`console/lib/discussion/types.ts`) are identical:

```json
{
  "id": "rollback-release",
  "label": "Roll back the release",
  "description": "Revert 1.7.2, redeploy 1.7.1, reopen the incident",
  "recommended": true
}
```

| Field | Required | Semantics |
|---|---|---|
| `id` | yes | Stable machine key, unique within the card. Answers come back as `selectedOptionIds` — ids are the ONLY join key between what the agent offered and what the board chose. Never a display string; never reused inside one card. |
| `label` | yes | Short human display ("Roll back the release"). One line; imperative voice for actions. |
| `description` | no | One line of deciding context: what happens, who acts, what it unblocks. Not a paragraph — long context belongs on the card's `detailsMarkdown`, not on every option. |
| `recommended` | no | The author's pick. Renders as a badge + preselect. At most ONE recommended option on `choose_one` cards; any subset on `choose_many`. Zero is legal (the board decides on its own). |

Unknown fields are ignored, not rejected — additive evolution only (see §7).

## 3. Card-level rules (both kinds)

1. **A choice is always a choice:** `choose_one`/`choose_many` cards carry
   **at least 2 options**, enforced server-side by
   `DecisionRequestPayload.Validate` (WS-6.1 / ISI-7223). A single-option
   "choice" is not a choice.
2. **Sweet spot 2–5, hard cap 9:** author 2–5 options; `Validate` rejects
   more than 9 because the FE binds number keys 1–9 (a 10th option has no
   key and breaks the keyboard contract).
3. **Explicit opt-outs only:** `approve` (genuine binary) and `free_form`
   (open answer) are the legal ways to NOT offer options. They carry none by
   construction. "One option + approve-ish framing" to dodge rule 1 is a
   contract violation.
4. **Answers reference ids, never labels.** Labels are display-only and may
   be reworded freely; ids are immutable once the card is posted.
5. **Reject stays first-class:** every card remains rejectable with reason
   unless the author explicitly opts out (`allowReject: false`); approve
   mode always permits rejection (ADR-0026 §4.4).

## 4. Rendering contract (inbox)

`console/components/inbox/DecisionCard.tsx` is the SINGLE renderer for this
taxonomy — suggest-next-steps cards reuse it, they do not get a bespoke
component. The contract (WS-6.4 / ISI-7226):

- letter chips a/b/c… per option, in payload order;
- `recommended` renders as a badge and is preselected;
- number keys 1–9 pick, Enter submits (a11y radiogroup/checkbox group);
- `choose_one` answers inline from the inbox row;
- after decision the card is read-only — the audit trail lives in the thread.

## 5. Suggest-next-steps mapping (ISI-7075 C4 / ISI-7218)

A coordinator that reports blocked/stuck raises a suggest-next-steps card.
Each proposed next action is ONE option in the shared shape:

- `id` — stable slug of the action (`create-child-is-7231`, `reassign-to-cto`);
- `label` — the imperative action ("Create ISI child for X", "Reassign Y to CTO");
- `description` — what it does, who owns it, what it unblocks;
- `recommended` — the coordinator's pick, at most one.

Rules specific to this kind:

1. "Blocked with zero suggested next steps" is a contract violation — if the
   coordinator genuinely sees no path, the card must say so in
   `detailsMarkdown` and offer `free_form` (the board fills the path).
2. Suggestion options describe actions to take, not questions to answer.
3. Cross-system parity: the Paperclip interaction kinds
   (`suggest_tasks`, `ask_user_questions`, `request_confirmation`) map onto
   the same taxonomy — task proposals are options in this shape, questions
   are `choose_one`/`free_form` cards, confirmations are `approve` cards —
   so a board member moves between the two systems without relearning the
   interaction grammar.

## 6. Enforcement points

| Layer | Where | Issue |
|---|---|---|
| Server | `DecisionRequestPayload.Validate` (>=2, cap 9, unique ids, id+label) | ISI-7223 (WS-6.1) |
| Runtime helper | `RequestDecision` — single validation point for every emitter (plan default O2) | ISI-7224 (WS-6.2) |
| Prompts | cross-runtime contract: "raise a decision request with 2–5 labeled options, never a bare yes/no" | ISI-7225 (WS-6.3) |
| Frontend | DecisionCard rendering + E2E multi-option cards | ISI-7226 (WS-6.4) |
| This doc | the taxonomy itself, shared by both kinds | ISI-7227 (WS-6.5) |

The >=2 floor holds even if prompts drift, because the helper and server
enforce it (defense in depth: prompt → helper → server).

## 7. Change policy

- Additive field additions: allowed without an ADR; unknown fields are ignored by consumers (see §2).
- Changes to existing field semantics, the >=2/cap-9 rules, or the
  id-as-join-key contract: require a new ADR superseding this section.
- This doc is normative for both kinds; a workstream that needs to deviate
  updates THIS doc first, then its code.
