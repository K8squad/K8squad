package contextasm

// boarddecision.go — ISI-7225 (ISI-7074 WS-6.3, plan ISI-7204 rev 1): the
// cross-runtime board-decision prompt contract. WS-6.2 (ISI-7224) landed the
// emission path — the task-io request-decision verb riding the shared
// DecisionRaise helper with its server-side >=2-option floor — but a verb
// nobody knows about starves the inbox DecisionCard exactly like zero
// emitters did. This constant is stamped into EVERY assembled envelope as an
// authoritative directive (buildEnvelope), so the contract holds for every
// runtime (OpenClaw, Hermes, Claude Code, OpenCode, ...) with no per-runtime
// prompt drift: one wording, one place.
//
// The contract is behavioral, not mechanical: the >=2 floor and the 2..5
// recommended band are ENFORCED server-side by the WS-6.2 helper (mirrored by
// the store gate, WS-6.1/ISI-7223), so this text drifting can weaken the
// wording, never the floor — prompts teach agents to raise cards SHAPED
// right (labeled options, one-line description each, a recommendation,
// never a bare yes/no or an open question), the helper guarantees the
// mechanics. approve/free_form are the helper's explicit opt-outs — a
// confirmation of an already-written proposal / a genuinely unstructured
// ask — not escape hatches from composing options.
const boardDecisionContract = `Board decisions — the ksquad contract (all runtimes): when a decision belongs to the board — anything beyond your delegated authority, a scope or priority trade-off, or a CoordinatorMode=propose gate that needs the board to pick a path — raise a decision request via the task-io request-decision verb (POST on your task-io endpoint; the run credential and base URL are already in your environment). Shape: a one-line title, brief context, and 2-5 options — each a one-line, concretely different course of action — with recommended set to your own pick (1-based option number). Never a bare yes/no: mode approve exists only to confirm an already-written proposal. Never an open question: mode free_form exists only for a genuinely unstructured ask, not to dodge composing options. If you cannot name at least two concrete options, you do not have a decision yet — keep working and narrow the choice first. The board answers on the card and your run resumes on the choice.`
