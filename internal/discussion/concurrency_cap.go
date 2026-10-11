// ISI-5670 (ISI-5662 WS2) — the HARD per-discussion concurrent-run safety backstop.
//
// A discussion room turns every agent turn into a separate PAID Run. The existing bounds each guard a
// DIFFERENT axis and none of them bounds the TOTAL in-flight runs a single discussion can have at once:
//
//   - MaxVoicesPerRound (party.go) caps the voices SELECTED per facilitator round, and only when the
//     sequenced party path actually engages. A degraded/legacy dispatch path never consults it.
//   - The ISI-5594 per-endpoint FIFO gate (pkg/controller/rundrive/endpoint_gate.go) serializes runs
//     that share ONE endpoint URL — scoped to an endpoint, not to a discussion.
//   - The legacy bare-party broadcast arm (dispatch.go) historically fanned out up to 25 runs per post.
//
// So a degraded path — no coordinator, an explicit broadcast, a facilitator directive that slipped back
// to the old one-run-per-team-agent shape — could still fan a whole squad out in parallel. This cap is
// the backstop that holds REGARDLESS of which dispatch path runs: at most MaxConcurrentRunsPerDiscussion
// in-flight runs per discussion thread, enforced at the single run-minting chokepoint every path funnels
// through (internal/mentiondispatch.DispatchMention), and mirrored onto the broadcast SELECTION cap so a
// party broadcast never even picks more targets than the discussion is allowed to run.

package discussion

import (
	"os"
	"strconv"
	"strings"
)

// DefaultMaxConcurrentRunsPerDiscussion is the hard safety ceiling on simultaneously in-flight agent
// runs per discussion thread (ISI-5670), independent of the per-round voice bound and the per-endpoint
// FIFO gate. 4 mirrors MaxVoicesPerRound so a single well-behaved round never trips the backstop, while a
// degraded fan-out (legacy broadcast, no-coordinator path) is still held to the same ceiling.
const DefaultMaxConcurrentRunsPerDiscussion = 4

// MaxConcurrentRunsEnv is the env/Secret key that overrides the per-discussion concurrent-run cap. It is
// read once at package init (set it on the apiserver, operator, and memory pod envs via the Helm chart /
// a Secret). A missing, non-numeric, or non-positive value falls back to the default — the cap is never
// silently disabled by a bad override.
const MaxConcurrentRunsEnv = "KSQUAD_DISCUSSION_MAX_CONCURRENT_RUNS"

// maxConcurrentRunsPerDiscussion is the live cap, resolved from the environment at init. It is a package
// var (not a const) so the value is configurable per deployment and so in-package tests can exercise a
// specific ceiling; production code reads it only through MaxConcurrentRunsPerDiscussion().
var maxConcurrentRunsPerDiscussion = resolveMaxConcurrentRunsFromEnv(os.Getenv(MaxConcurrentRunsEnv))

// resolveMaxConcurrentRunsFromEnv parses an override value, falling back to the default for anything that
// is not a positive integer. Pure (the raw string is passed in) so it is exhaustively unit-testable.
func resolveMaxConcurrentRunsFromEnv(raw string) int {
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxConcurrentRunsPerDiscussion
}

// MaxConcurrentRunsPerDiscussion returns the configured hard per-discussion concurrent-run cap (default
// DefaultMaxConcurrentRunsPerDiscussion, override MaxConcurrentRunsEnv). Both the run-minting backstop
// (internal/mentiondispatch) and the broadcast selection cap (resolveMentionTargets) read it, so the two
// are always the SAME limit (ISI-5670 req 1+2).
func MaxConcurrentRunsPerDiscussion() int { return maxConcurrentRunsPerDiscussion }
