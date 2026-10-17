// ticketkey.go — ISI-6088 (ISI-6055 P1 follow-up): the ONE canonical definition
// of the ksquad.ticket.key= work-item label convention, shared by the writers
// (the creation paths that stamp the label) and the reader (rundrive's
// resolveTicketKey, which derives the ksquad.ticket.ref span attribute at
// dispatch). Before this file the prefix existed only as a resolution-order
// literal on the read side, so a minter stamping the label had no constant to
// share and the two sides could drift.
//
// WHAT THE LABEL IS: the work item's HUMAN ticket key — the board-readable
// identifier a person uses to find the ticket this item services (an external
// board key like ISI-6040, or a repo ref like owner/repo#42). It is SPAN-ATTRIBUTION
// data only: the dispatch leg resolves it and carries it on the submit payload
// so the pod stamps ksquad.ticket.ref on run.start / llm.call / gen_ai.tool.call /
// mcp.call / run.end (ISI-6061). Never a metric label (NFR-OBS3).
//
// NEVER FABRICATED: the label is stamped ONLY where the creation path holds a
// real external key. The minting-path coverage (ISI-6088):
//
//   - GitHub issue bridge (console assign + auto issue triage): stamps
//     ksquad.github.issue=<owner>/<repo>#N, the SECOND resolution convention —
//     resolves without ksquad.ticket.key.
//   - PR-review triage (reviewtrigger → reviewdispatch): carries the plaintext
//     ksquad.github.pr=<owner>/<repo>#N anchor (ISI-4767 E5), the THIRD
//     resolution convention since ISI-6088.
//   - CI-failure triage (cifailure → cifailuredispatch): stamps
//     ksquad.ticket.key=<owner>/<repo>@<sha12>/<check-name> at creation; "" (and
//     therefore key-less) when the repo URL has no sluggable owner/repo.
//   - Console create (POST /api/projects/{pid}/work-items): the optional
//     ticketKey request field; coord.CreateWorkItem stamps it as this label.
//   - KEY-LESS BY DESIGN (no external key exists at mint time, so none is
//     invented): mention-dispatch drive items, party facilitation/takeaways
//     items, proposal-confirm minted items. Their runs correlate through the
//     opaque ksquad.work_item.ref uuid, which the shim always stamps.
package coord

// TicketKeyLabelPrefix is the canonical label prefix carrying the work item's
// human ticket key. First in rundrive's resolution order.
const TicketKeyLabelPrefix = "ksquad.ticket.key="

// TicketKeyLabel builds the canonical ticket-key label for key. The caller
// guarantees key is non-empty and trimmed; the label still passes the normal
// create-field bounds (maxLabelLen) in normalizeCreateFields, so an over-long
// key fails closed as ErrInvalidWorkItem (400), never a silent truncation.
func TicketKeyLabel(key string) string {
	return TicketKeyLabelPrefix + key
}

// GitHubIssueLabelPrefix is the canonical label prefix carrying the GitHub-issue
// join/dedup key (ISI-4757 Epic-2/Epic-4 bridge): ksquad.github.issue=<owner>/<repo>#N.
// It is BOTH the idempotency label the issue→ticket writers stamp (the apiserver
// manual assign bridge, githubissuedispatch.go, and the auto issue-triage
// trigger, issuetrigger) AND the second convention in rundrive's
// ticketKeyLabelPrefixes resolution order. Shared here (ISI-6124, closing the
// same writer/reader drift risk TicketKeyLabelPrefix closed for
// ksquad.ticket.key= in ISI-6088) so a trigger renaming or re-spelling its
// anchor can no longer silently desynchronize the resolver. Create-time
// work-item attribute, span attribution only, never a metric label (NFR-OBS3).
const GitHubIssueLabelPrefix = "ksquad.github.issue="

// GitHubPRLabelPrefix is the canonical label prefix carrying the PR-review
// anchor (ISI-4767 E5): ksquad.github.pr=<owner>/<repo>#N. It is the plaintext
// join label reviewtrigger's PRAnchorLabel stamps and the third convention in
// rundrive's ticketKeyLabelPrefixes resolution order — a PR shares the repo's
// issue number space, so it resolves exactly like the issue convention. Shared
// here (ISI-6124) for the same drift-proofing reason. Create-time work-item
// attribute, span attribution only, never a metric label (NFR-OBS3).
const GitHubPRLabelPrefix = "ksquad.github.pr="
