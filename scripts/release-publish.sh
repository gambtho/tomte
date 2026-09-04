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
# WHICH ARTIFACTS. Not "all of them": a 1ES build publishes symbols,
# logs, SBOMs and compliance output beside the binaries, and everything
# attached here lands on a public release. Two filters, both explicit:
#
#   ADO_ARTIFACTS  comma-separated Azure DevOps artifact NAMES to consider
#                  (empty = every artifact the builds published)
#   ASSET_GLOBS    comma-separated globs matched against each FILE's
#                  basename inside those artifacts. Default is the
#                  installer extensions this project ships.
#
# Everything considered is printed with its size and marked kept or
# skipped, and `--list` does that and stops. The driver runs `--list`
# BEFORE asking for an approval, so the person approving has seen the
# exact asset list rather than a count they have to trust.
#
# Usage (via scripts/release-run.sh STEP=publish):
#   GITHUB_REPO=owner/name VERSION=v1.2.3 NOTES_FILE=notes.md \
#   ADO_ORG=org ADO_PROJECT=proj ADO_BUILDS=1,2,3 [ADO_ARTIFACTS=...] \
#   [ASSET_GLOBS='*.dmg,*.exe'] release-publish.sh [--list]
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
ado_artifacts="${ADO_ARTIFACTS:-}"
# The installer extensions this project ships. Deliberately a list of
# what IS wanted rather than of what is not: a new compliance artifact
# appearing in a build must not silently become a release asset.
asset_globs="${ASSET_GLOBS:-*.dmg,*.exe,*.deb,*.tar.gz,*.AppImage,*.msi,*.rpm}"
list_only=0
[ "${1:-}" = "--list" ] && list_only=1
api="https://dev.azure.com/$ado_org/$ado_project/_apis"

command -v az >/dev/null || { echo 'az is required to read Azure DevOps artifacts' >&2; exit 1; }
command -v gh >/dev/null || { echo 'gh is required to create the GitHub release' >&2; exit 1; }
command -v unzip >/dev/null || { echo 'unzip is required to unpack an Azure DevOps artifact' >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
step() { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }
note() { printf '   %s\n' "$*" >&2; }

# matches_glob <basename> — true when the file is one this release ships.
matches_glob() {
  local base=$1 g
  local IFS=,
  for g in $asset_globs; do
    g="${g// /}"
    [ -n "$g" ] || continue
    # shellcheck disable=SC2053 # a glob on the right is the point
    case "$base" in ($g) return 0 ;; esac
  done
  return 1
}

# A token minted NOW, not one captured at the start of the session. The
# transfer takes minutes and the plane's stored token lives about an
# hour; this step is the one that would meet that deadline.
#
# AND FOR THE RIGHT RESOURCE. This talks to the Azure DevOps REST API at
# dev.azure.com, which is NOT the resource the MCP server is
# (https://mcp.dev.azure.com). A token minted for the MCP scope is
# refused here — as a 302 to a sign-in page rather than a 401, which is
# how it presented the first time and is worth recognising: an HTML
# redirect where JSON was expected means the token was not accepted, not
# that the URL was wrong.
#
# ADO_API_SCOPE overrides. The default is the Azure DevOps resource's URI
# form; if your tenant only accepts the application-id form, pass it —
# that identifier is not committed here, because this repository refuses
# to carry Azure identifiers even public ones
# (scripts/check-no-azure-ids.sh).
ADO_API_SCOPE="${ADO_API_SCOPE:-https://app.vssps.visualstudio.com/.default}"
step "Minting an Azure DevOps API token for the transfer"
az account get-access-token --scope "$ADO_API_SCOPE" \
  --query accessToken -o tsv > "$work/ado.tok" 2>"$work/ado.err" || {
  cat "$work/ado.err" >&2
  echo "could not mint a token for $ADO_API_SCOPE" >&2
  echo 'Pass ADO_API_SCOPE=<azure devops application id>/.default if your tenant' >&2
  echo 'wants the application-id form instead of the resource URI.' >&2
  exit 1; }
test -s "$work/ado.tok" || { echo 'az returned an empty token' >&2; exit 1; }
{ printf 'Authorization: Bearer '; cat "$work/ado.tok"; printf '\n'; } > "$work/ado.hdr"

# --- 0. did those builds actually succeed? -----------------------------
#
# Checked FIRST, because a failed build's artifacts are the wrong thing to
# put on a release and "no installers were produced" is a confusing way to
# learn that a build failed. On the first real run two of three builds hit
# Azure DevOps' 60-minute job cap during signing, and the only symptom
# here was an empty asset list.
step "Checking those builds succeeded"
IFS=',' read -ra builds <<< "$ado_builds"
bad=0
for b in "${builds[@]}"; do
  b="${b// /}"; [ -n "$b" ] || continue
  [[ "$b" =~ ^[0-9]+$ ]] || { echo "invalid build id '$b'" >&2; exit 2; }
  curl -sS -H @"$work/ado.hdr" -H 'Accept: application/json' \
    -o "$work/build-$b.json" -w '%{http_code}' \
    "$api/build/builds/$b?api-version=7.1" > "$work/status" || true
  st=$(cat "$work/status")
  if [ "$st" = 302 ] || [ "$st" = 401 ]; then
    echo "the Azure DevOps REST API refused this token (HTTP $st) — see ADO_API_SCOPE" >&2; exit 1
  fi
  [ "$st" = 200 ] || { echo "reading build $b answered HTTP $st" >&2; exit 1; }
  read -r bstatus bresult bname < <(python3 - "$work/build-$b.json" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
print(d.get("status", "?"), d.get("result", "?"),
      (d.get("definition") or {}).get("name", "?").replace(" ", "_"))
EOF
)
  case "$bstatus/$bresult" in
    completed/succeeded) note "OK        $b  ${bname//_/ }" ;;
    completed/partiallySucceeded)
      note "PARTIAL   $b  ${bname//_/ } — partially succeeded"
      [ "${ALLOW_PARTIAL:-0}" = 1 ] || bad=1 ;;
    *) note "NOT OK    $b  ${bname//_/ } — status=$bstatus result=$bresult"; bad=1 ;;
  esac
done
if [ "$bad" != 0 ]; then
  echo >&2
  echo 'refusing to publish from builds that did not succeed.' >&2
  echo 'Re-run the builds, then publish with the new build ids.' >&2
  echo '(ALLOW_PARTIAL=1 accepts a partially-succeeded build.)' >&2
  exit 1
fi

# --- 1. every artifact the named builds published ----------------------
step "Reading the artifacts those builds published"
: > "$work/plan"
for b in "${builds[@]}"; do
  b="${b// /}"
  [ -n "$b" ] || continue
  curl -sS -H @"$work/ado.hdr" -H 'Accept: application/json' \
    -o "$work/arts-$b.json" -w '%{http_code}' \
    "$api/build/builds/$b/artifacts?api-version=7.1" > "$work/status" || true
  status=$(cat "$work/status")
  if [ "$status" = 302 ] || [ "$status" = 401 ]; then
    echo "the Azure DevOps REST API refused this token (HTTP $status)." >&2
    echo "The token was minted for $ADO_API_SCOPE, which is not the resource" >&2
    echo "dev.azure.com accepts. Re-run with ADO_API_SCOPE set to the Azure" >&2
    echo "DevOps resource your tenant issues for (its URI or application-id form)." >&2
    exit 1
  fi
  [ "$status" = 200 ] || { echo "listing artifacts for build $b answered HTTP $status" >&2
                           head -c 300 "$work/arts-$b.json" >&2; exit 1; }
  ADO_ARTIFACTS="$ado_artifacts" python3 - "$work/arts-$b.json" "$b" >> "$work/plan" <<'EOF'
import json, os, sys
d = json.load(open(sys.argv[1]))
want = {n.strip() for n in os.environ.get("ADO_ARTIFACTS", "").split(",") if n.strip()}
for a in d.get("value", []):
    name = a.get("name", "")
    if want and name not in want:
        print("skip-artifact\t%s\t%s" % (sys.argv[2], name), file=sys.stderr)
        continue
    url = (a.get("resource") or {}).get("downloadUrl")
    if url:
        print("%s\t%s\t%s" % (sys.argv[2], name, url))
EOF
done
test -s "$work/plan" || { echo 'those builds published no artifacts' >&2; exit 1; }

# The FILES inside those artifacts, listed WITHOUT downloading them: an
# artifact's resource.data is "#/<containerId>/<name>", and the container
# API returns every item with its size. 1.28 GB is too much to move just
# to find out what is in it, and the person approving needs the list
# before they decide, not after.
step "What is in them"
: > "$work/files"
while IFS=$'\t' read -r b name url; do
  cid=$(python3 - "$work/arts-$b.json" "$name" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
for a in d.get("value", []):
    if a.get("name") == sys.argv[2]:
        data = (a.get("resource") or {}).get("data") or ""
        if data.startswith("#/"):
            print(data.split("/")[1])
        break
EOF
)
  if [ -z "$cid" ]; then
    note "build $b: $name — not a container artifact; its files will be listed after download"
    printf '%s\t%s\t?\t%s\n' "$b" "$name" "(unlisted)" >> "$work/files"
    continue
  fi
  curl -sS -H @"$work/ado.hdr" -H 'Accept: application/json' -o "$work/items.json" \
    "https://dev.azure.com/$ado_org/_apis/resources/Containers/$cid?itemPath=$name&isShallow=false&api-version=7.1-preview.4" \
    >/dev/null || true
  python3 - "$work/items.json" "$b" "$name" >> "$work/files" <<'EOF'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
for it in d.get("value", []):
    if it.get("itemType") != "file":
        continue
    path = it.get("path", "")
    print("%s\t%s\t%s\t%s" % (sys.argv[2], sys.argv[3], it.get("fileLength", 0), path.rsplit("/", 1)[-1]))
EOF
done < "$work/plan"

# Print the decision, file by file, before anything is created or moved.
kept=0
while IFS=$'\t' read -r b name size base; do
  if [ "$base" = "(unlisted)" ]; then note "?      build $b  $name (contents unknown until downloaded)"; continue; fi
  hsize=$(numfmt --to=iec --suffix=B "$size" 2>/dev/null || echo "${size}B")
  if matches_glob "$base"; then note "KEEP   $base  ($hsize, build $b / $name)"; kept=$((kept + 1))
  else note "skip   $base  ($hsize, build $b / $name)"; fi
done < "$work/files"
note ""
note "$kept file(s) match ASSET_GLOBS=$asset_globs"

# The empty case is a FAILURE in both modes, and that matters more in
# --list than in the real run: --list is what the driver calls BEFORE it
# asks a human, so exiting 0 here meant the driver went on to request
# approval for a release with no assets. The real run would still have
# refused — the same check, one line down — but a person had already been
# asked to approve something that could never happen.
if [ "$kept" -eq 0 ]; then
  echo 'no file matched ASSET_GLOBS — there is nothing to publish.' >&2
  echo 'Either the builds have not produced installers yet, or ASSET_GLOBS' >&2
  echo "does not describe them. Current globs: $asset_globs" >&2
  exit 1
fi
if [ "$list_only" = 1 ]; then
  note "--list: nothing was created and nothing was moved."
  exit 0
fi

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
    if [ ! -s "$f" ]; then note "skip (empty)      $base"; continue; fi
    if ! matches_glob "$base"; then
      note "skip (not an asset) $base ($(du -h "$f" | cut -f1))"
      continue
    fi
    note "UPLOAD            $base ($(du -h "$f" | cut -f1))"
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
