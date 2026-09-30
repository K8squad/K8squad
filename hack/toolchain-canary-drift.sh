#!/usr/bin/env bash
# toolchain-canary-drift.sh — CI drift guard for the all-tools canary (ISI-5222).
#
# Uses bash (process substitution); it is a repo-side CI/dev guard, not part of
# the sandbox Run. The canary body (hack/toolchain-canary.sh) stays POSIX sh.
#
# Fails if the canary's authoritative tool list (hack/toolchain-canary.sh) and
# the Skill fixture (examples/toolchain-canary/skill.yaml) drift from the set of
# Dockerfile.toolchain-* images in the repo. Adding a new toolchain image
# therefore forces a matching canary update — no tool can be silently missed as
# new toolchains are added (ISI-5222 acceptance requirement).
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

# Ground truth: one line per Dockerfile.toolchain-<name>.
dockerfile_tools=$(ls Dockerfile.toolchain-* 2>/dev/null \
	| sed 's|^Dockerfile.toolchain-||' | sort -u)

# What the canary script declares (TOOLS="..." line).
canary_tools=$(sed -n 's/^TOOLS="\(.*\)"$/\1/p' hack/toolchain-canary.sh \
	| tr ' ' '\n' | grep -v '^$' | sort -u)

# What the Skill fixture declares (name@version toolchain refs).
skill_tools=$(sed -n 's/.*- \([a-z0-9-]*\)@.*/\1/p' examples/toolchain-canary/skill.yaml \
	| sort -u)

status=0
diff_set() {
	label=$1; want=$2; have=$3
	missing=$(comm -23 <(printf '%s\n' "$want") <(printf '%s\n' "$have"))
	extra=$(comm -13 <(printf '%s\n' "$want") <(printf '%s\n' "$have"))
	if [ -n "$missing" ]; then
		printf 'DRIFT: %s is missing tools present as Dockerfile.toolchain-*:\n%s\n' "$label" "$missing"
		status=1
	fi
	if [ -n "$extra" ]; then
		printf 'DRIFT: %s declares tools with no Dockerfile.toolchain-*:\n%s\n' "$label" "$extra"
		status=1
	fi
}

diff_set "canary script (hack/toolchain-canary.sh)" "$dockerfile_tools" "$canary_tools"
diff_set "Skill fixture (examples/toolchain-canary/skill.yaml)" "$dockerfile_tools" "$skill_tools"

if [ "$status" -eq 0 ]; then
	n=$(printf '%s\n' "$dockerfile_tools" | grep -c .)
	printf 'OK: canary + Skill fixture cover all %s Dockerfile.toolchain-* images.\n' "$n"
fi
exit "$status"
