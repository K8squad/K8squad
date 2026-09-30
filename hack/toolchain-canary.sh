#!/bin/sh
# toolchain-canary.sh — all-tools sandbox canary (ISI-5222, parent ISI-5219).
#
# End-to-end validation that the skill->toolchain init-pack path works: proves
# EVERY registered toolchain binary is present + functional inside a live
# warm-pool sandbox after ISI-5221 (the layer-5 assembly/attach fix,
# main@80af771b) is deployed.
#
# This is the body of the canary Run. The Run's Skill (examples/toolchain-canary/
# skill.yaml) declares all 14 toolchain refs so the operator stages every
# init pack onto /tools/bin (PATH). This script then version-probes each tool
# and FAILS the Run (exit 1) if any binary is missing or non-functional.
#
# It is deliberately POSIX sh + self-contained: it runs unchanged inside the
# alpine-based sandbox (busybox sh) with no repo checkout required.
#
# Re-run: see docs/runbooks/toolchain-canary.md.
set -u

# ---------------------------------------------------------------------------
# Authoritative tool list. MUST stay in lockstep with Dockerfile.toolchain-*
# and images/toolchains/matrix.json. The in-repo drift guard
# (hack/toolchain-canary-drift.sh, wired in CI) fails if this list and the
# Dockerfile.toolchain-* glob diverge, so a newly-added toolchain cannot be
# silently missed by the canary.
# ---------------------------------------------------------------------------
TOOLS="git curl gh dtctl kubectl helm jq yq python node go uv make docker-cli"

pass=0
fail=0
failed_tools=""

log()  { printf '%s\n' "$*"; }
ok()   { pass=$((pass + 1)); log "  PASS  $1 -> $2"; }
bad()  { fail=$((fail + 1)); failed_tools="$failed_tools $1"; log "  FAIL  $1 -> $2"; }

# probe <tool> <binary> <version-args...>
# Resolves the binary (with go's non-PATH install dir as a fallback) and runs
# its version command, capturing the first output line as evidence.
probe() {
	tool=$1
	bin=$2
	shift 2
	path=$(command -v "$bin" 2>/dev/null)
	if [ -z "$path" ]; then
		# go stages under /usr/local/go/bin, which is not on PATH by default.
		for cand in /usr/local/go/bin/"$bin" /tools/bin/"$bin"; do
			[ -x "$cand" ] && path=$cand && break
		done
	fi
	if [ -z "$path" ]; then
		bad "$tool" "binary '$bin' not found on PATH"
		return
	fi
	out=$("$path" "$@" 2>&1 | head -n1)
	rc=$?
	if [ $rc -ne 0 ]; then
		bad "$tool" "'$bin $*' exited $rc: ${out:-<no output>}"
		return
	fi
	ok "$tool" "${out:-<ok>}"
}

log "== ISI-5222 all-tools sandbox canary =="
log "expected toolchains: $TOOLS"
log ""
log "-- version probes --"

probe git       git     --version
probe curl      curl    --version
probe gh        gh      --version
probe dtctl     dtctl   version
probe kubectl   kubectl version --client
probe helm      helm    version --short
probe jq        jq      --version
probe yq        yq      --version
probe python    python3 --version
probe node      node    --version
probe go        go      version
probe uv        uv      --version
probe make      make    --version
probe docker-cli docker --version

log ""
log "-- functional checks --"

# git: real clone + read history (the original ISI-5219 repro).
CLONE_URL="https://github.com/isItObservable/sympozium-todo-demo.git"
CLONE_DIR="${TMPDIR:-/tmp}/canary-sympozium-$$"
if git clone --depth 1 "$CLONE_URL" "$CLONE_DIR" >/dev/null 2>&1 &&
	git -C "$CLONE_DIR" log -1 --oneline >/dev/null 2>&1; then
	head=$(git -C "$CLONE_DIR" log -1 --oneline 2>/dev/null)
	ok "git-clone" "$CLONE_URL @ $head"
else
	bad "git-clone" "clone or 'git log -1' failed for $CLONE_URL"
fi
rm -rf "$CLONE_DIR" 2>/dev/null

# curl: fetch a known-good URL (headers only, fail on HTTP >= 400).
if curl -sSf -o /dev/null -I https://github.com >/dev/null 2>&1; then
	ok "curl-fetch" "GET https://github.com ok"
else
	bad "curl-fetch" "curl -sSf https://github.com failed"
fi

log ""
log "== summary: ${pass} passed, ${fail} failed =="
if [ "$fail" -ne 0 ]; then
	log "MISSING/BROKEN:${failed_tools}"
	log "RESULT: FAIL"
	exit 1
fi
log "RESULT: PASS — every skill-toolchain present + functional"
exit 0
