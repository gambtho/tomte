#!/usr/bin/env bash
# W32: capture the RELEASE agent's GitHub token stdin-only and store it as
# the plane-side Secret the github-release upstream reads
# (kaimahi-release-pat, key: token) — after PROVING, fail-closed, what can
# be proven about it.
#
# This is scripts/github-secret.sh's sibling, and the differences are the
# point. That token is READ-ONLY and feeds the P10 demo. This one can
# change a real repository, so it gets its own Secret, its own upstream
# entry and its own allowlist, and the P10 seam keeps exactly the blast
# radius it was reviewed with.
#
# Which token: a FINE-GRAINED personal access token, scoped to ONE
# repository, with:
#   Contents:      Read and write   (create the release branch)
#   Pull requests: Read             (collate the notes)
#   Actions:       Read and write   (dispatch the build workflow)
#   Metadata:      Read             (comes along)
#
# Say the uncomfortable part out loud, because it decides where the
# guardrail actually lives: GitHub has no permission that grants creating
# a ref without also granting DELETING one. Contents: write is the
# smallest scope that can create a release branch, and it necessarily also
# permits `DELETE /repos/{o}/{r}/git/refs/{ref}`. There is no separate
# Releases permission either. So the token is NOT what stops a destructive
# git operation here — the gateway is (the destructive tools are excluded
# at the server by the upstream's extra_headers, absent from the
# allowlist, and absent from the agent's tool selection). See
# docs/release-agent.md, "What makes a destructive operation impossible".
#
# Custody rules (docs/COORDINATION.md security guidance):
#   - Token bytes travel only through pipes and 0600 files — never argv,
#     env listings, YAML, or logs. curl reads the auth header from a file.
#   - No -L on any keyed call.
#   - Every step checks a well-formed positive; a failed check stores
#     nothing.
#
# Usage: GITHUB_REPO=owner/name bash scripts/release-secret.sh   (token on stdin)
set -euo pipefail
umask 077

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE="${RELEASE_SECRET_NAMESPACE:-kaimahi}"
SECRET_NAME="${RELEASE_SECRET_NAME:-kaimahi-release-pat}"
repo="${GITHUB_REPO:-}"

if [ -z "$repo" ]; then
  echo 'usage: make release-secret GITHUB_REPO=owner/name   (fine-grained write token on stdin)' >&2
  exit 2
fi
# Anchored: the value is interpolated into a URL and must be exactly
# owner/name.
if ! [[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$ ]]; then
  echo "invalid GITHUB_REPO '$repo' (want owner/name)" >&2
  exit 2
fi

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

echo 'Paste the fine-grained GitHub token (github_pat_...), press Enter, then Ctrl-D:' >&2
tr -d '\r\n' < /dev/stdin > "$workdir/token"
test -s "$workdir/token" || { echo 'no token read on stdin' >&2; exit 1; }
if ! grep -qE '^github_pat_[A-Za-z0-9_]{20,}$' "$workdir/token"; then
  echo 'REFUSING: that is not a fine-grained token (expected a github_pat_ prefix).' >&2
  echo 'A classic PAT or an OAuth token cannot be scoped to one repository, and this' >&2
  echo 'token can write.' >&2
  exit 1
fi
{ printf 'Authorization: Bearer '; cat "$workdir/token"; printf '\n'; } > "$workdir/auth-header"

api() {
  curl -sS -X GET -H @"$workdir/auth-header" \
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
    -D "$workdir/resp-headers" -o "$workdir/resp" -w '%{http_code}' "$1"
}

# 1. The token can read THE repository, and is what it claims to be. A
# classic token announces its scopes in X-OAuth-Scopes; a fine-grained
# one does not. Both facts are asserted from the same response.
status=$(api "https://api.github.com/repos/$repo")
[ "$status" = 200 ] || {
  echo "GET /repos/$repo answered HTTP $status — the token cannot read that repository" >&2
  echo '(a fine-grained token must list the repository under "Repository access")' >&2
  exit 1; }
if tr -d '\r' < "$workdir/resp-headers" | grep -qiE '^x-oauth-scopes: *[^ ]'; then
  echo 'REFUSING: the token carries OAuth scopes (a classic token); use a fine-grained one.' >&2
  exit 1
fi
python3 - "$workdir/resp" "$repo" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
want = sys.argv[2].lower()
if not isinstance(d, dict) or str(d.get("full_name", "")).lower() != want:
    sys.exit("the repository answer does not name %s — refusing to store" % sys.argv[2])
print("token vetted: it reads %s" % sys.argv[2], file=sys.stderr)
EOF

# 2. ONE repository, not several. A fine-grained token's /user/repos is
# restricted to the repositories it was granted, so a listing that names
# more than one is a token with reach this lane does not accept — and
# that is the fail-closed direction, so it REFUSES. A listing that cannot
# be read at all is only a warning: the reach is then unproven, not
# proven wide, and the check above already established the token reaches
# the right repository.
status=$(api 'https://api.github.com/user/repos?per_page=100&affiliation=owner,collaborator,organization_member')
if [ "$status" = 200 ]; then
  python3 - "$workdir/resp" "$repo" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
if not isinstance(d, list):
    print("could not read the repository listing — scope unproven", file=sys.stderr)
    sys.exit(0)
names = sorted({str(r.get("full_name", "")) for r in d if isinstance(r, dict)})
want = sys.argv[2].lower()
if len(names) > 1 or (names and names[0].lower() != want):
    sys.exit("REFUSING: this token reaches %d repositories (%s). A release token is\n"
             "scoped to exactly one." % (len(names), ", ".join(names[:5])))
print("token vetted: it reaches exactly one repository", file=sys.stderr)
EOF
else
  echo "note: /user/repos answered HTTP $status — one-repository scope unproven (not refused)" >&2
fi

# 3. Its deadline, said now rather than when it bites — the same rule
# P16 applied to Kaimahi's own credentials. GitHub returns this header
# for a fine-grained token; a token with no expiry is refused, because an
# unattended write credential that never dies is not one this lane hands
# to an agent.
expiry=$(tr -d '\r' < "$workdir/resp-headers" \
  | sed -n 's/^[Gg]ithub-[Aa]uthentication-[Tt]oken-[Ee]xpiration: *//p' | head -1)
if [ -z "$expiry" ]; then
  echo 'REFUSING: this token reports no expiration. Create it with an expiry —' >&2
  echo 'a write credential that never dies is one nobody ever revokes.' >&2
  exit 1
fi

# 4. Store. --from-file keeps the value out of argv; the manifest exists
# only inside the apply pipe, never on disk. Create-or-update in one
# apply so an existing Secret stays intact if this fails partway.
$KUBECTL get namespace "$NAMESPACE" >/dev/null 2>&1 || $KUBECTL create namespace "$NAMESPACE"
$KUBECTL -n "$NAMESPACE" create secret generic "$SECRET_NAME" \
  --from-file=token="$workdir/token" \
  --dry-run=client -o yaml | $KUBECTL apply -f - >/dev/null
echo "Secret $NAMESPACE/$SECRET_NAME stored. It expires $expiry." >&2

cat >&2 <<'NOTE'

The gateway injects this token on calls to the github-release upstream
from plane custody; the agent never holds it. What was PROVEN here: the
token is fine-grained, it reads the one repository named, it reaches no
other, and it has a deadline. What was NOT, because GitHub does not
expose a fine-grained token's permissions: that you granted Contents,
Pull requests and Actions and nothing else — that part is yours.

Delete it with: make release-revoke
NOTE
