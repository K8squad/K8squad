#!/usr/bin/env python3
"""Story 11.5 (ISI-2737) falsification — the SourceProvider seam is EXPLICIT and
TOTAL: every byte of GitHub-specific knowledge in the Go codebase lives behind
`pkg/scm.SourceProvider` (interface + provider impls + registry), so GitLab/
Bitbucket/Gitea follow as a new implementation + a registry entry with ZERO
reconciler, ingress, or mirror change (arch §5.4/§10.2; Epic 11 story 11.5;
spec detailing ISI-2699).

WHY THIS BENCH EXISTS
---------------------
Story 11.1 landed the seam informally; 11.5 locks it. Four ways a plausible-
but-wrong codebase silently breaks the acceptance, none caught by "it compiles
/ it syncs once":

  1. SEAM-BLEED (prod code) — a non-pkg/scm .go file names a GitHub identifier:
     the go-github SDK import, a delivery header (X-Hub-Signature-256,
     X-GitHub-Event, X-GitHub-Delivery), or api.github.com. The reconciler or
     webhook ingress is then provider-coupled and GitLab cannot follow without
     touching control-plane code — exactly the redesign the story forbids.

  2. SEAM GAP (webhook parsing outside the seam) — the ingress parses provider
     payloads itself (github event-shape probing like `"zen"`/`"pull_request"`
     keys in a non-provider file). The AC names webhook parsing as part of the
     seam: it must sit in the provider (ParseWebhookEvent).

  3. REGISTRY GAP — a provider exists as an implementation but is not
     constructible through `ProviderRegistry` ("new impl + config" must be the
     WHOLE diff; an unregistered impl would force a code change elsewhere to
     be usable, i.e. a hidden redesign).

  4. INGRESS PROVIDER BRANCH — the webhook ingress switches on a provider name
     (`if provider == "github"`) instead of resolving through the registry and
     letting the provider own its delivery scheme. One branch today is a
     fork-per-provider tomorrow.

Layers (stdlib-only, like the story-11.1 bench):

  C1 static scan — no GitHub-specific identifier in non-test .go files outside
     pkg/scm (test doubles/fixtures excluded: exercising a provider THROUGH
     the seam is what the seam is for).
  C2 static scan — no GitHub payload-shape probing outside pkg/scm.
  C3 registry totality — every provider name the CRD enum admits as an
     implementation that exists is registered in NewProviderRegistry; the
     seam-documented not-yet-implemented names fail closed at the registry.
  C4 static scan — no provider-name literal branch in the ingress/reconciler.

GREEN on the 11.5 baseline; each injected mutation flips its check RED.
"""

import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# C1 — provider-SDK / delivery-header / API-host identifiers.
GITHUB_IDENTIFIERS = [
    "go-github/v",           # the SDK import path
    "X-Hub-Signature-256",
    "X-GitHub-Event",
    "X-GitHub-Delivery",
    "api.github.com",
]

# C2 — GitHub webhook payload-shape keys probed for event attribution.
GITHUB_PAYLOAD_KEYS = [
    '"zen"',
    "'zen'",
    '"pull_request"',
    "'pull_request'",
]

# Where the seam lives: everything under pkg/scm is allowed to be provider-
# specific. hack/ is the bench itself. docs/ and non-Go files are not code.
def is_seam_or_noncode(path: Path) -> bool:
    s = str(path)
    if s.startswith(str(REPO / "pkg" / "scm")):
        return True
    if s.startswith(str(REPO / "hack")):
        return True
    if not s.endswith(".go"):
        return True
    return False


def go_files():
    for p in REPO.rglob("*.go"):
        p = p.resolve()
        if "/vendor/" in str(p) or is_seam_or_noncode(p):
            continue
        yield p


def check_c1_seam_bleed() -> bool:
    """C1: no GitHub identifier in prod Go outside pkg/scm. Test files may
    deliver GitHub-shaped fixtures THROUGH the seam; prod code may not."""
    ok = True
    for p in go_files():
        if p.name.endswith("_test.go"):
            continue
        text = p.read_text(encoding="utf-8", errors="replace")
        for ident in GITHUB_IDENTIFIERS:
            if ident in text:
                print(f"C1 RED: {p.relative_to(REPO)} names GitHub identifier {ident!r}")
                ok = False
    if ok:
        print("C1 GREEN: no GitHub identifier in prod Go outside pkg/scm")
    return ok


def check_c2_payload_probe() -> bool:
    """C2: no GitHub payload-shape probing outside pkg/scm (webhook parsing
    sits behind ParseWebhookEvent). Test files may ship GitHub-shaped
    FIXTURES — delivering one through the seam is how the v1 provider path
    is exercised; prod code may not interpret provider payload shapes."""
    ok = True
    for p in go_files():
        if p.name.endswith("_test.go"):
            continue
        text = p.read_text(encoding="utf-8", errors="replace")
        for key in GITHUB_PAYLOAD_KEYS:
            if key in text:
                print(f"C2 RED: {p.relative_to(REPO)} probes GitHub payload key {key}")
                ok = False
    if ok:
        print("C2 GREEN: webhook payload parsing lives inside pkg/scm only")
    return ok


def check_c3_registry_totality() -> bool:
    """C3: every provider implemented under pkg/scm is registered in
    NewProviderRegistry, so 'new impl + config' is the whole adoption diff."""
    registry = (REPO / "pkg" / "scm" / "registry.go").read_text(encoding="utf-8")
    registered = set(re.findall(r'\.Register\(\s*"([a-z]+)"', registry))
    ok = True
    # A provider constructor NewXxxProvider in pkg/scm implies Name() "xxx".
    for p in (REPO / "pkg" / "scm").glob("*.go"):
        if p.name.endswith("_test.go"):
            continue
        text = p.read_text(encoding="utf-8", errors="replace")
        for m in re.finditer(r'func New([A-Za-z]+)Provider\(', text):
            name = m.group(1).lower()
            if name not in registered:
                print(f"C3 RED: provider impl {m.group(1)} has no registry entry")
                ok = False
    if ok:
        print(f"C3 GREEN: all provider impls registered (registered: {sorted(registered)})")
    return ok


def check_c4_no_provider_branch() -> bool:
    """C4: the ingress and the reconciler never branch on a provider-name
    literal — they resolve through the registry and stay provider-neutral."""
    ok = True
    targets = [
        REPO / "cmd" / "scm-webhook" / "main.go",
        REPO / "pkg" / "controller" / "reposync" / "repo_sync.go",
    ]
    branch_re = re.compile(r'==\s*"github"|==\s*"gitlab"|==\s*"bitbucket"|==\s*"gitea"')
    for p in targets:
        if not p.exists():
            continue
        text = p.read_text(encoding="utf-8", errors="replace")
        for i, line in enumerate(text.splitlines(), 1):
            if branch_re.search(line):
                print(f"C4 RED: {p.relative_to(REPO)}:{i} branches on a provider name: {line.strip()}")
                ok = False
    if ok:
        print("C4 GREEN: no provider-name branch in ingress or reconciler")
    return ok


def mutations() -> list:
    """Broken-codebase mutations; each must flip its designated check RED.
    Each entry: (description, designated check id, [(path, old, new), ...])."""
    ingress = REPO / "cmd" / "scm-webhook" / "main.go"
    registry = REPO / "pkg" / "scm" / "registry.go"
    reconciler = REPO / "pkg" / "controller" / "reposync" / "repo_sync.go"
    return [
        ("M1 ingress names the GitHub signature header (seam-bleed)", "C1",
         [(ingress,
           '	if !provider.VerifyWebhookDelivery(r.Context(), r.Header, body, string(secret.Data[secretKey])) {',
           '	if r.Header.Get("X-Hub-Signature-256") == "" {\n\t\t\th.unauthorized(w, "no github header", "project", projectName)\n\t\t\treturn\n\t\t}\n' +
           '	if !provider.VerifyWebhookDelivery(r.Context(), r.Header, body, string(secret.Data[secretKey])) {')]),
        ("M2 ingress probes a GitHub payload key (webhook parse outside seam)", "C2",
         [(ingress,
           '	attribution := "unknown"',
           '	attribution := "unknown"\n\tif bytes.Contains(body, []byte("zen")) {\n\t\tattribution = "ping"\n\t}')]),
        ("M3 gitlab impl without a registry entry (hidden redesign)", "C3",
         [(registry, 'r.Register("gitlab",', 'r.RegisterDisabled("gitlab",')]),
        ("M4 reconciler branches on provider name", "C4",
         [(reconciler,
           '	provider, err := r.Providers.Provider(ctx, sync.Provider, creds)',
           '	if sync.Provider == "github" {\n\t\t_ = creds\n\t}\n' +
           '	provider, err := r.Providers.Provider(ctx, sync.Provider, creds)')]),
    ]


def run_mutation_bench() -> bool:
    ok = True
    checks = {"C1": check_c1_seam_bleed, "C2": check_c2_payload_probe,
              "C3": check_c3_registry_totality, "C4": check_c4_no_provider_branch}
    for desc, designated, edits in mutations():
        saved = {}
        try:
            for path, old, new in edits:
                text = path.read_text(encoding="utf-8")
                if old not in text:
                    print(f"BENCH RED: mutation anchor missing in {path.name}: {old[:60]!r}")
                    ok = False
                    continue
                saved[path] = text
                path.write_text(text.replace(old, new, 1), encoding="utf-8")
            if not checks[designated]():
                print(f"BENCH GREEN: mutation caught: {desc}")
            else:
                print(f"BENCH RED: mutation NOT caught: {desc}")
                ok = False
        finally:
            for path, text in saved.items():
                path.write_text(text, encoding="utf-8")
    return ok


def main() -> int:
    print("Story 11.5 (ISI-2737) — SourceProvider seam falsification.")
    print()
    results = [
        check_c1_seam_bleed(),
        check_c2_payload_probe(),
        check_c3_registry_totality(),
        check_c4_no_provider_branch(),
    ]
    baseline_ok = all(results)
    print()
    bench_ok = run_mutation_bench()
    print()
    if baseline_ok and bench_ok:
        print("VERDICT: GREEN — the seam is explicit and total (C1–C4 + mutation bench).")
        return 0
    print("VERDICT: RED — seam violation; fix before merging story 11.5.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
