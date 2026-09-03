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
# (scripts/check-no-azure-ids.sh). The constraint is applied to the
# running plane from a parameter.
#
# CONSEQUENCE, said plainly: `make plane` applies the committed table and
# will drop this. Re-run it after. (An overlay that survives a plane apply
# is what P15 is for; it is not merged, and this lane does not depend on
# unmerged work.)
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

here="$(cd "$(dirname "$0")/.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

probe_ctx=$($KUBECTL config view --minify -o jsonpath='{.contexts[0].name}')
KUBE_NS="kaimahi" KUBE_CTX="$probe_ctx" bash "$here/scripts/kube-guard.sh" "release-bind $repo"

$KUBECTL -n kaimahi get configmap kaimahi-upstreams -o jsonpath='{.data.upstreams\.json}' > "$work/live.json"
test -s "$work/live.json" || { echo 'the plane is not deployed (no kaimahi-upstreams ConfigMap)' >&2; exit 1; }

python3 - "$work/live.json" "$CRED_RELEASE" "$repo" <<'EOF' > "$work/patched.json"
import json, sys

live, cred, repo = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(live))

# ADD to whatever the table already carries, never replace it: since P13
# that block holds the accounts-payable agent's real constraint, and a
# patch that dropped it would quietly govern a different plane than the
# one that ships.
sc = c.setdefault("standing_constraints", {})
if repo == "-":
    sc.pop(cred, None)
    if not sc:
        c.pop("standing_constraints", None)
    json.dump(c, sys.stdout, indent=2)
    print("", file=sys.stderr)
    sys.exit(0)

owner, name = repo.split("/", 1)
# Only the tools that READ. Anything consequential must stay outside every
# constraint so that it is denied and approved, call by call.
read_tools = ["list_pull_requests", "list_commits", "list_tags", "list_releases",
              "get_latest_release", "get_release_by_tag", "actions_list", "actions_get"]
declared = {}
for up in c.get("tool_upstreams", {}).values():
    declared.update(up.get("tools", {}))
missing = [t for t in read_tools if t not in declared]
if missing:
    sys.exit("the running table does not declare %s — is it older than this script?"
             % ", ".join(missing))

sc[cred] = {t: [{"field": "owner", "op": "eq", "value": owner},
                {"field": "repo", "op": "eq", "value": name}]
            for t in read_tools}
json.dump(c, sys.stdout, indent=2)
EOF

$KUBECTL -n kaimahi create configmap kaimahi-upstreams --from-file=upstreams.json="$work/patched.json" \
  --dry-run=client -o yaml | $KUBECTL apply -f - >/dev/null
$KUBECTL -n kaimahi rollout restart deploy/kaimahi-proxy >/dev/null
$KUBECTL -n kaimahi rollout status deploy/kaimahi-proxy --timeout=300s >/dev/null

if [ "$repo" = "-" ]; then
  echo "release-bind: the repository binding for $CRED_RELEASE is removed." >&2
else
  echo "release-bind: $CRED_RELEASE may now read only $repo." >&2
  echo "  A read naming any other repository is denied and files a request." >&2
  echo "  Consequential calls are unaffected: they are approved one at a time." >&2
  echo "  Re-run this after 'make plane', which restores the committed table." >&2
fi
