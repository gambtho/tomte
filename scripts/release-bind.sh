#!/usr/bin/env bash
# W32: bind the release credential to ONE repository, at the plane.
#
# The release agent's read tools are on its allowlist, which says WHICH
# TOOLS it may call and says nothing about which repository it may call
# them on. This adds the missing half as a P12 standing constraint: the
# read tools are callable only with owner=<owner> and repo=<name>, and a
# call naming any other repository is denied and files a request, exactly
# as an unlisted tool does.
#
# WHY ONLY THE READ TOOLS. A standing constraint ADMITS: a call inside its
# bounds proceeds with no human. That is right for reading a repository
# and would be exactly wrong for create_branch or actions_run_trigger,
# which must be approved every time. Those carry no constraint, are on no
# allowlist, and are therefore denied on every attempt — which is the
# design, not an omission.
#
# WHY THIS IS NOT COMMITTED. `Azure/aks-desktop` is somebody's project,
# and the committed table in a public repository is the wrong place for
# it — the same reason no Azure identifier is committed
# (scripts/check-no-azure-ids.sh). It is operator config, so it goes
# where P15 put operator config: the OVERLAY ConfigMap
# (kaimahi-upstreams-extra), as one fragment.
#
# That is the whole reason to use the overlay rather than patching the
# committed table in place: `make plane` reapplies the base and would
# drop an in-place patch, and a repository binding that silently
# disappears on the next deploy is worse than none. The overlay survives
# it. The merge is per-name and refuses collisions, so this fragment sits
# beside the accounts-payable agent's constraint without touching it.
#
# The fragment is written with `kubectl patch --type merge` on the one
# key, never a create-or-replace of the whole ConfigMap: another
# operator's fragments live in there too.
#
# Usage: GITHUB_REPO=owner/name bash scripts/release-bind.sh
#        GITHUB_REPO=- bash scripts/release-bind.sh     (remove the binding)
set -euo pipefail
umask 077

KUBECTL="${KUBECTL:-kubectl}"
CRED_RELEASE="${CRED_RELEASE:-release-agent}"
repo="${GITHUB_REPO:-}"

[ -n "$repo" ] || { echo 'usage: make release-bind GITHUB_REPO=owner/name  (or GITHUB_REPO=- to remove)' >&2; exit 2; }
if [ "$repo" != "-" ] && ! [[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$ ]]; then
  echo "invalid GITHUB_REPO '$repo' (want owner/name, or - to remove)" >&2
  exit 2
fi

OVERLAY_CM="${OVERLAY_CM:-kaimahi-upstreams-extra}"
FRAGMENT="${FRAGMENT:-release-bind.json}"

here="$(cd "$(dirname "$0")/.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

probe_ctx=$($KUBECTL config view --minify -o jsonpath='{.contexts[0].name}')
KUBE_NS="kaimahi" KUBE_CTX="$probe_ctx" bash "$here/scripts/kube-guard.sh" "release-bind $repo"

# The BASE table has to be readable, and has to declare the tools the
# constraint names: a constraint on an undeclared tool refuses the whole
# config at boot, which would take the plane down rather than fail this
# command. Check it here, where the failure is one message.
$KUBECTL -n kaimahi get configmap kaimahi-upstreams -o jsonpath='{.data.upstreams\.json}' > "$work/base.json"
test -s "$work/base.json" || { echo 'the plane is not deployed (no kaimahi-upstreams ConfigMap)' >&2; exit 1; }

if [ "$repo" = "-" ]; then
  # Remove just this fragment. A null value in a merge patch DELETES the
  # key, leaving every other operator's fragment alone.
  $KUBECTL -n kaimahi patch configmap "$OVERLAY_CM" --type merge \
    -p "{\"data\": {\"$FRAGMENT\": null}}" >/dev/null 2>&1 || true
else
  python3 - "$work/base.json" "$CRED_RELEASE" "$repo" <<'EOF' > "$work/fragment.json"
import json, sys

base, cred, repo = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(base))
owner, name = repo.split("/", 1)

# Only the tools that READ. Anything consequential must stay outside every
# constraint so that it is denied and approved, call by call — a standing
# constraint ADMITS, and admitting a branch creation would be the opposite
# of the design.
read_tools = ["list_pull_requests", "list_commits", "list_tags", "list_releases",
              "get_latest_release", "get_release_by_tag", "actions_list", "actions_get"]
declared = {}
for up in c.get("tool_upstreams", {}).values():
    declared.update(up.get("tools") or {})
missing = [t for t in read_tools if t not in declared]
if missing:
    sys.exit("the running table declares no policy fields for %s — is the plane older "
             "than this script? (make plane)" % ", ".join(missing))
if cred in (c.get("standing_constraints") or {}):
    sys.exit("the committed table already constrains %r; refusing to shadow it from an "
             "overlay (the merge would be refused at boot anyway)" % cred)

# The fragment carries ONLY standing_constraints. An overlay may not set
# an upstream's custody fields, and this needs none of them.
json.dump({"standing_constraints": {cred: {
    t: [{"field": "owner", "op": "eq", "value": owner},
        {"field": "repo", "op": "eq", "value": name}] for t in read_tools}}},
    sys.stdout, indent=2)
EOF

  # Create the overlay ConfigMap if this is the first fragment, then set
  # OUR key with a merge patch — never a create-or-replace, because other
  # operators' fragments live in the same ConfigMap.
  $KUBECTL -n kaimahi get configmap "$OVERLAY_CM" >/dev/null 2>&1 \
    || $KUBECTL -n kaimahi create configmap "$OVERLAY_CM" >/dev/null
  python3 - "$work/fragment.json" "$FRAGMENT" <<'EOF' > "$work/patch.json"
import json, sys
print(json.dumps({"data": {sys.argv[2]: open(sys.argv[1]).read()}}))
EOF
  $KUBECTL -n kaimahi patch configmap "$OVERLAY_CM" --type merge --patch-file "$work/patch.json" >/dev/null
fi

# The overlay is read at boot, so the change is a rollout. A fragment the
# plane refuses takes the rollout down and leaves the serving replicas
# up — the same fail-closed shape every other config error has.
$KUBECTL -n kaimahi rollout restart deploy/kaimahi-proxy >/dev/null
if ! $KUBECTL -n kaimahi rollout status deploy/kaimahi-proxy --timeout=300s >/dev/null; then
  echo "release-bind: the proxy did not roll out — the plane refused the overlay." >&2
  $KUBECTL -n kaimahi logs -l app=kaimahi-proxy --tail=20 >&2 || true
  echo "  The previously serving replicas are still up. Remove the fragment with:" >&2
  echo "    make release-bind GITHUB_REPO=-" >&2
  exit 1
fi

if [ "$repo" = "-" ]; then
  echo "release-bind: the repository binding for $CRED_RELEASE is removed." >&2
else
  echo "release-bind: $CRED_RELEASE may now read only $repo." >&2
  echo "  A read naming any other repository is denied and files a request." >&2
  echo "  Consequential calls are unaffected: they are approved one at a time." >&2
  echo "  It lives in the overlay ($OVERLAY_CM/$FRAGMENT), so 'make plane' keeps it." >&2
fi
