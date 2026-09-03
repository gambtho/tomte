#!/usr/bin/env bash
# Wait for a REAL person to approve one request by typing in Slack.
#
# WHY THIS EXISTS. scripts/ap-demo.sh and scripts/ap-injection.sh already
# had two ways to approve: the admin bearer, and — with SLACK_USER set —
# scripts/slack-mention-probe.sh, which SYNTHESISES a correctly signed
# app_mention as that user id. The synthetic one is right on kind and in
# CI, where Slack cannot reach the cluster and the id is invented
# (U0CIAPPROVER). It is exactly wrong against a real workspace: forging a
# signed event that claims a named colleague approved a payment is not a
# demonstration of a human approving a payment, it is a demonstration of
# how to fake one.
#
# So on a cluster Slack can actually reach, the scenarios hand the
# decision to this script instead. NOTHING HERE APPROVES ANYTHING. It
# prints the line for a person to type, waits for the plane to record
# THEIR decision — arriving the P8a way, over the internet through the
# edge, and parsed the P8b way as an app-mention command — and fails
# closed if the decision never comes, comes from somebody else, or is a
# denial.
#
# Usage:  ap-await-approval.sh <request id> <slack user id> [uses]
#   env: KUBECTL             kubectl invocation incl. --context
#        CRED_AP             the credential whose grants are checked
#        AP_HUMAN_TIMEOUT    seconds to wait (default 900)
#        AP_HUMAN_POLL       seconds between polls (default 5)
set -euo pipefail
umask 077

KUBECTL="${KUBECTL:-kubectl}"
CRED_AP="${CRED_AP:-ap-agent}"
TIMEOUT="${AP_HUMAN_TIMEOUT:-900}"
POLL="${AP_HUMAN_POLL:-5}"
export KUBECTL

id="${1:?usage: ap-await-approval.sh <request id> <slack user id> [uses]}"
user="${2:?usage: ap-await-approval.sh <request id> <slack user id> [uses]}"
uses="${3:-1}"

case "$user" in
  (*[!A-Z0-9]*|'') echo "invalid Slack user id '$user' (want uppercase alphanumerics)" >&2; exit 2 ;;
esac
case "$TIMEOUT$POLL" in
  (*[!0-9]*|'') echo "AP_HUMAN_TIMEOUT and AP_HUMAN_POLL must be whole seconds" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

admin() { bash "$here/plane-admin.sh" "$@"; }

# What is pending, read BEFORE the wait: the subject (the tool) is what
# the checks below match on, and once the request is decided it is no
# longer in this list to be read from.
admin approvals > "$work/pending.out"
subject=$(awk -v id="$id" '$1==id {print $5; exit}' "$work/pending.out")
if [ -z "$subject" ]; then
  cat "$work/pending.out" >&2
  echo "ap-await-approval: request $id is not pending — nothing to wait for" >&2
  exit 1
fi

printf '\n\033[1m   WAITING FOR A HUMAN.\033[0m This is the request, as the plane states it:\n\n' >&2
grep -F "$id" "$work/pending.out" >&2 || true
printf '\n   In the Slack channel this cluster posts to, %s types:\n\n' "$user" >&2
printf '       @kaimahi approve %s uses=%s ttl=10m\n\n' "${id%%-*}" "$uses" >&2
printf '   Nothing here can do that for them. This script watches the plane for\n' >&2
printf '   THEIR decision and gives up after %ss.\n\n' "$TIMEOUT" >&2

deadline=$(( $(date +%s) + TIMEOUT ))
decided=no
while [ "$(date +%s)" -lt "$deadline" ]; do
  # A read that FAILED is not an answer. Only a successful listing may
  # decide anything: treating a transient admin-read error as "the
  # request is gone" would report an approval nobody gave.
  if admin approvals > "$work/pending.new" 2>/dev/null; then
    mv "$work/pending.new" "$work/pending.out"
    if ! awk -v id="$id" '$1==id {found=1} END{exit !found}' "$work/pending.out"; then
      decided=yes
      break
    fi
  fi
  sleep "$POLL"
done

if [ "$decided" != yes ]; then
  echo "ap-await-approval: request $id was still pending after ${TIMEOUT}s." >&2
  echo "  Not claiming an approval. Check that the Slack app's Request URL points" >&2
  echo "  at this cluster's edge, and that $user is in the approver file" >&2
  echo "  (make slack-approvers)." >&2
  exit 1
fi

# Decided is not approved, and approved is not approved BY THAT PERSON.
# Both are read from the plane's own records rather than inferred from the
# request having left the pending list — a denial empties it too.
admin approval-audit "$CRED_AP" > "$work/audit.out"
if ! awk -v cred="$CRED_AP" -v subj="$subject" -v who="slack:$user" \
  '$2==cred && $3=="tool" && $4==subj && $5=="approved" && $6==who {found=1} END{exit !found}' \
  "$work/audit.out"; then
  cat "$work/audit.out" >&2
  echo "ap-await-approval: no 'approved' row for $subject decided by slack:$user." >&2
  echo "  The request was decided, but not approved by that person — treated as a" >&2
  echo "  failure rather than continuing on somebody else's authority." >&2
  exit 1
fi

admin grants "$CRED_AP" > "$work/grants.out"
if ! awk -v cred="$CRED_AP" -v subj="$subject" -v who="slack:$user" \
  '$2==cred && $3=="tool" && $4==subj && index($0, who) {found=1} END{exit !found}' \
  "$work/grants.out"; then
  cat "$work/grants.out" >&2
  echo "ap-await-approval: $subject was approved by slack:$user but carries no grant." >&2
  exit 1
fi

printf '   \033[1m%s approved by slack:%s\033[0m — recorded by the plane, with a grant.\n' \
  "$subject" "$user" >&2
