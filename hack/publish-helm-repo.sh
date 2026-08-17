#!/usr/bin/env bash
# Publish the packaged Helm chart(s) + quickstart.yaml to the gh-pages Helm repo
# served at https://charts.k8squad.io (ISI-2632).
#
# Idempotent: re-runs merge new chart versions into the existing index.yaml and
# never rewrite gh-pages history. On the very first publish it creates the
# orphan gh-pages branch. GitHub Pages must be enabled for this repo with source
# = gh-pages branch, and a DNS CNAME charts.k8squad.io -> <owner>.github.io must
# exist (admin/DNS-owner task — see the CNAME file this script writes).
set -euo pipefail

REPO="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY (owner/name) is required}"
GH_TOKEN="${GH_TOKEN:?GH_TOKEN with contents:write is required}"
DOMAIN="${PAGES_DOMAIN:-charts.k8squad.io}"
PKG_DIR="${PKG_DIR:-.cr-release-packages}"
QUICKSTART="${QUICKSTART:-dist/quickstart.yaml}"
REPO_URL="https://${DOMAIN}"

remote="https://x-access-token:${GH_TOKEN}@github.com/${REPO}.git"
work="$(mktemp -d)"

if git ls-remote --exit-code --heads "$remote" gh-pages >/dev/null 2>&1; then
  git clone --branch gh-pages --single-branch "$remote" "$work"
else
  echo "gh-pages does not exist yet — creating orphan branch."
  git clone "$remote" "$work"
  git -C "$work" switch --orphan gh-pages
  git -C "$work" rm -rf . >/dev/null 2>&1 || true
fi

cp "$PKG_DIR"/*.tgz "$work"/

# Merge new packages into the repo index, preserving older chart versions.
if [ -f "$work/index.yaml" ]; then
  helm repo index "$work" --url "$REPO_URL" --merge "$work/index.yaml"
else
  helm repo index "$work" --url "$REPO_URL"
fi

cp "$QUICKSTART" "$work/quickstart.yaml"
printf '%s\n' "$DOMAIN" > "$work/CNAME"

# Minimal human landing page so the apex is not a bare 404.
cat > "$work/index.html" <<HTML
<!doctype html><meta charset="utf-8"><title>K8squad Helm charts</title>
<h1>K8squad Helm repository</h1>
<pre>helm repo add ksquad https://${DOMAIN}
helm repo update
helm install ksquad ksquad/k8squad --namespace k8squad-system --create-namespace</pre>
<p><a href="index.yaml">index.yaml</a> &middot; <a href="quickstart.yaml">quickstart.yaml</a></p>
HTML

cd "$work"
git add -A
if git diff --cached --quiet; then
  echo "gh-pages already up to date — nothing to publish."
  exit 0
fi
git -c user.name="k8squad-ci" -c user.email="ci@k8squad.io" \
  commit -m "chore(helm-repo): publish charts + quickstart.yaml [skip ci]"
git push "$remote" gh-pages
echo "Published to ${REPO_URL} (index.yaml, quickstart.yaml, CNAME=${DOMAIN})."
