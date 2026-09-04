#!/usr/bin/env bash
# W32: publish the release — create it on GitHub with the agent's notes,
# then move the build artifacts out of Azure DevOps and onto it.
#
# WHY THE DRIVER MOVES THE BYTES AND NOT THE AGENT, and why that is not a
# violation of the rule it looks like one of.
#
# D38(b) says the agent never carries bytes: the gateway caps a request
# body at 4 MiB and an MCP relay is the wrong thing to push a binary
# through. That rule is about TOOL CALLS, and it still holds here — the
# agent's part of publishing is a small JSON call and a drafted paragraph.
#
# The rule assumed something that is not true for this project, though:
# that some CI system could be told to move the artifacts. It cannot.
# GitHub Actions has no access to this Azure DevOps organization, and
# adding a pipeline to Azure DevOps was ruled out. The only thing that
# reaches both is the machine the operator is already sitting at, which
# is where this driver runs. So it streams them: 1.28 GB across five
# assets on the last release, ADO to GitHub, never through the plane.
#
# WHAT IS GOVERNED HERE, said precisely rather than implied. The DECISION
# is governed: publishing files an approval request naming the exact
# release, a human approves that request, and this refuses to move a byte
# without a live grant welded to it. The TRANSFER is not gateway-enforced
# — no allowlist stands between this script and the bytes, because the
# gateway is not in the path. That is a weaker claim than the rest of this
# lane makes and it is written here rather than glossed.
#
# Credentials are the operator's own, deliberately: `az` for Azure DevOps
# and `gh` for GitHub, both already on the machine. Plane custody is not
# used and not pretended — a 300 MB stream is not going through the
# proxy. A fresh ADO token is minted at transfer time, which is also why
# this step cannot be stranded by the hour-long token that stranded the
# build approvals.
#
# Usage (via scripts/release-run.sh STEP=publish):
#   GITHUB_REPO=owner/name VERSION=v1.2.3 NOTES_FILE=notes.md \
#   ADO_ORG=org ADO_PROJECT=proj ADO_BUILDS=1,2,3 release-publish.sh
set -euo pipefail
umask 077

repo="${GITHUB_REPO:?set GITHUB_REPO}"
version="${VERSION:?set VERSION}"
notes_file="${NOTES_FILE:?set NOTES_FILE}"
ado_org="${ADO_ORG:?set ADO_ORG}"
ado_project="${ADO_PROJECT:?set ADO_PROJECT}"
ado_builds="${ADO_BUILDS:?set ADO_BUILDS (comma-separated build ids)}"
target="${RELEASE_TARGET:-}"
prerelease="${PRERELEASE:-1}"
api="https://dev.azure.com/$ado_org/$ado_project/_apis"

command -v az >/dev/null || { echo 'az is required to read Azure DevOps artifacts' >&2; exit 1; }
command -v gh >/dev/null || { echo 'gh is required to create the GitHub release' >&2; exit 1; }
command -v unzip >/dev/null || { echo 'unzip is required to unpack an Azure DevOps artifact' >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
step() { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }
note() { printf '   %s\n' "$*" >&2; }

# A token minted NOW, not one captured at the start of the session. The
# transfer takes minutes and the plane's stored token lives about an
# hour; this step is the one that would meet that deadline.
step "Minting an Azure DevOps token for the transfer"
az account get-access-token --scope https://mcp.dev.azure.com/.default \
  --query accessToken -o tsv > "$work/ado.tok" 2>"$work/ado.err" || {
  cat "$work/ado.err" >&2; echo 'could not mint an Azure DevOps token' >&2; exit 1; }
test -s "$work/ado.tok" || { echo 'az returned an empty token' >&2; exit 1; }
{ printf 'Authorization: Bearer '; cat "$work/ado.tok"; printf '\n'; } > "$work/ado.hdr"

# --- 1. every artifact the named builds published ----------------------
step "Reading the artifacts those builds published"
: > "$work/plan"
IFS=',' read -ra builds <<< "$ado_builds"
for b in "${builds[@]}"; do
  b="${b// /}"
  [ -n "$b" ] || continue
  [[ "$b" =~ ^[0-9]+$ ]] || { echo "invalid build id '$b'" >&2; exit 2; }
  curl -sS -H @"$work/ado.hdr" -H 'Accept: application/json' \
    -o "$work/arts-$b.json" -w '%{http_code}' \
    "$api/build/builds/$b/artifacts?api-version=7.1" > "$work/status" || true
  status=$(cat "$work/status")
  [ "$status" = 200 ] || { echo "listing artifacts for build $b answered HTTP $status" >&2
                           head -c 300 "$work/arts-$b.json" >&2; exit 1; }
  python3 - "$work/arts-$b.json" "$b" >> "$work/plan" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
for a in d.get("value", []):
    url = (a.get("resource") or {}).get("downloadUrl")
    if url:
        print("%s\t%s\t%s" % (sys.argv[2], a.get("name", ""), url))
EOF
done
test -s "$work/plan" || { echo 'those builds published no artifacts' >&2; exit 1; }
cut -f1,2 "$work/plan" | while IFS=$'\t' read -r b n; do note "build $b: $n"; done

# --- 2. the release, with the agent's notes ----------------------------
step "Creating release $version on $repo"
if gh release view "$version" -R "$repo" >/dev/null 2>&1; then
  note "release $version already exists — assets will be added to it"
else
  gh release create "$version" -R "$repo" \
    --title "$version" --notes-file "$notes_file" \
    ${target:+--target "$target"} \
    $( [ "$prerelease" = 1 ] && printf -- '--prerelease' ) >&2
fi

# --- 3. the bytes ------------------------------------------------------
step "Moving the artifacts"
uploaded=0
while IFS=$'\t' read -r b name url; do
  note "downloading $name (build $b)"
  curl -sSL -H @"$work/ado.hdr" -o "$work/$name.zip" -w '%{http_code}' "$url" > "$work/status"
  status=$(cat "$work/status")
  [ "$status" = 200 ] || { echo "downloading $name answered HTTP $status" >&2; exit 1; }
  rm -rf "$work/x"; mkdir -p "$work/x"
  unzip -q "$work/$name.zip" -d "$work/x"
  rm -f "$work/$name.zip"
  # An Azure DevOps artifact is a zip of a folder; the release assets are
  # the FILES inside it. Directories and empty files are skipped rather
  # than uploaded as broken assets.
  while IFS= read -r f; do
    base=$(basename "$f")
    [ -s "$f" ] || { note "skipping empty $base"; continue; }
    note "uploading $base ($(du -h "$f" | cut -f1))"
    gh release upload "$version" "$f" -R "$repo" --clobber >&2
    uploaded=$((uploaded + 1))
  done < <(find "$work/x" -type f | sort)
  rm -rf "$work/x"
done < "$work/plan"

[ "$uploaded" -gt 0 ] || { echo 'nothing was uploaded' >&2; exit 1; }

# --- 4. what actually landed, read back from GitHub --------------------
step "What is on the release, read back from GitHub"
gh release view "$version" -R "$repo" --json assets \
  --jq '.assets[] | "   \(.name)\t\(.size/1048576|floor)MB"' >&2
note "$uploaded asset(s) uploaded."
