#!/usr/bin/env bash
# W32: cut a release, with an agent doing the work and a human approving
# it (docs/release-agent.md, D38).
#
# ONE COMMAND, AND THE MACHINE DOES THE WAITING. That is the whole design
# constraint, and it came from the person this was built for: "the entire
# point of this is for me to not run commands myself, otherwise what is
# the point of the agent?" So this script is not a checklist to work
# through. It runs every step, blocks on the approvals itself, polls the
# builds itself, and interrupts a person exactly once per consequential
# call — to approve it, from Slack or from the operator's chair.
#
# WHAT THE AGENT DOES, and it is narrow (D38(1)): it reads what shipped
# last, reads what merged since, and DRAFTS the notes — the one place
# judgement earns its keep. Then it proposes each consequential call. It
# never decides to ship, and it never carries a byte: the release
# artifacts are built and published by the GitHub workflow and the Azure
# DevOps pipelines it dispatches.
#
# WHAT THIS SCRIPT DOES THAT THE AGENT CANNOT BE TRUSTED TO. Between the
# agent proposing a call and a human being asked to approve it, this
# script checks that the filed request is THE CALL THAT WAS ASKED FOR —
# every policy-relevant field, matched against what the operator typed on
# the command line. A model that proposed a different branch, a different
# workflow or a different repository files a request too, and it would
# look identical in `make approvals`. The P13 lane learned this the
# expensive way: on its first live run the agent's own turn filed a
# payment for a different invoice at the same amount, and the approval
# landed on the wrong one. So: no human is shown a request this script
# did not first match against the intent.
#
# Usage:
#   make release GITHUB_REPO=owner/name VERSION=v1.2.3 [options]
#
#   GITHUB_REPO      owner/name — the ONE repository (required)
#   VERSION          the release being cut, e.g. v1.2.3 (required)
#   BASE             branch the release is cut from (default main)
#   RELEASE_BRANCH   branch to create (default release/<VERSION>)
#   GH_WORKFLOW      a workflow file to dispatch, e.g. build-app-win.yml
#                    (repeatable, comma-separated; optional)
#   ADO_PROJECT      Azure DevOps project (optional; with ADO_PIPELINES)
#   ADO_PIPELINES    pipeline ids to queue, comma-separated (optional)
#   SLACK_USER       require THIS person's approval (default: any
#                    approver the plane accepts)
#   DRY_RUN=1        propose only: read, draft the notes, and stop before
#                    the first consequential call. Nothing is created.
#   STEP=<name>      run one step only: propose | cut | build | watch
#   WATCH_TIMEOUT    seconds to poll the builds (default 3600)
#   WATCH_POLL       seconds between polls (default 60)
#
# Azure identifiers never enter this repository: ADO_PROJECT and the
# organization are parameters with no committed default
# (scripts/check-no-azure-ids.sh).
set -euo pipefail
umask 077

KUBECTL="${KUBECTL:-kubectl}"
CRED_RELEASE="${CRED_RELEASE:-release-agent}"
SLACK_USER="${SLACK_USER:-}"
DRY_RUN="${DRY_RUN:-0}"
STEP="${STEP:-all}"
WATCH_TIMEOUT="${WATCH_TIMEOUT:-3600}"
WATCH_POLL="${WATCH_POLL:-60}"
GRANT_TTL="${GRANT_TTL:-15m}"
# The chat command, handed down by the Makefile so every turn lands on
# the SAME cluster as everything else. Word splitting is deliberate.
RELEASE_CHAT="${RELEASE_CHAT:-make chat AGENT=release-agent}"
# The driver polls the admin API while it waits for a human, through a
# port-forward — and the human's own `make approve` needs the same port.
# On the default (19091) the two collide, and the operator meets
# "address already in use" on the ONE command this script just told them
# to run. So the driver keeps its own port, and the default stays free
# for the person. Found the hard way, on the first real approval.
export ADMIN_PORT="${RELEASE_ADMIN_PORT:-19291}"
export KUBECTL

repo="${GITHUB_REPO:-}"
version="${VERSION:-}"
base="${BASE:-main}"
branch="${RELEASE_BRANCH:-release/${version}}"
gh_workflows="${GH_WORKFLOW:-}"
ado_project="${ADO_PROJECT:-}"
ado_pipelines="${ADO_PIPELINES:-}"
# The builds whose artifacts the release gets. Distinct from
# ADO_PIPELINES (definitions): these are RUN ids, known only after the
# builds have been queued.
ado_builds="${ADO_BUILDS:-}"
# The hosted Azure DevOps server is reached at its ORG-LESS URL, so every
# call must name the organization (its tools take orgName). It is an
# Azure identifier: passed in, never committed.
ado_org="${ADO_ORG:-}"

usage() { echo "usage: make release GITHUB_REPO=owner/name VERSION=v1.2.3 [BASE=main] [GH_WORKFLOW=...] [ADO_PROJECT=... ADO_PIPELINES=...]" >&2; exit 2; }
[ -n "$repo" ] || usage
[ -n "$version" ] || usage
[[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$ ]] \
  || { echo "invalid GITHUB_REPO '$repo' (want owner/name)" >&2; exit 2; }
# A tag-shaped version. Anchored because it is interpolated into a branch
# name and into every policy field this script matches on.
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$ ]] \
  || { echo "invalid VERSION '$version'" >&2; exit 2; }
[[ "$base" =~ ^[A-Za-z0-9._/-]{1,200}$ ]] || { echo "invalid BASE '$base'" >&2; exit 2; }
[[ "$branch" =~ ^[A-Za-z0-9._/-]{1,200}$ ]] || { echo "invalid RELEASE_BRANCH '$branch'" >&2; exit 2; }
[ -z "$ado_pipelines" ] || { [ -n "$ado_project" ] && [ -n "$ado_org" ]; } \
  || { echo "ADO_PIPELINES needs ADO_PROJECT and ADO_ORG" >&2; exit 2; }
[ -z "$ado_org" ] || [[ "$ado_org" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,62}$ ]] \
  || { echo "invalid ADO_ORG '$ado_org'" >&2; exit 2; }
case "$WATCH_TIMEOUT$WATCH_POLL" in (*[!0-9]*|'') echo "WATCH_TIMEOUT and WATCH_POLL must be whole seconds" >&2; exit 2 ;; esac

owner="${repo%%/*}"
name="${repo##*/}"

here="$(cd "$(dirname "$0")" && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

admin() { bash "$here/plane-admin.sh" "$@"; }

# refresh_ado — put a FRESH Azure DevOps token into plane custody.
#
# The credential is an Entra access token and it lives about an hour. A
# release session is longer than that: on the first real run it expired
# twice, and each time the visible symptom was the agent saying "there is
# no pipelines_build tool in my toolset" — because the seam had gone
# Accepted=False with Unauthorized and kagent had dropped its tools. The
# cause and the symptom are nowhere near each other.
#
# So the driver refreshes it rather than asking a person to. It runs on
# the operator's machine, `az` is already there, and the gateway reads
# the credential file per request, so no restart is needed — only the
# kubelet's projection delay. Capturing a credential is otherwise a
# human's job here (D27); this is the same capture, done on the
# operator's behalf at the moment it is needed, with the token never
# leaving a pipe.
refresh_ado() {
  [ -n "$ado_org" ] || return 0
  command -v az >/dev/null || { note "az not found — leaving the Azure DevOps token as it is"; return 0; }
  local tok; tok=$(mktemp); trap 'rm -f "$tok"' RETURN
  if ! az account get-access-token --scope https://mcp.dev.azure.com/.default \
        --query accessToken -o tsv > "$tok" 2>/dev/null || [ ! -s "$tok" ]; then
    note "could not mint an Azure DevOps token; the stored one stays in place"
    return 0
  fi
  $KUBECTL -n kaimahi create secret generic kaimahi-ado-token --from-file=token="$tok" \
    --dry-run=client -o yaml | $KUBECTL apply -f - >/dev/null
  note "Azure DevOps token refreshed in plane custody."
}

# ado_seam_ok — say what is actually wrong, rather than letting it reach
# the operator as "the agent has no such tool".
ado_seam_ok() {
  [ -n "$ado_org" ] || return 0
  local msg
  msg=$($KUBECTL -n kagent get remotemcpserver kaimahi-release-ado \
    -o jsonpath='{range .status.conditions[?(@.type=="Accepted")]}{.status}|{.message}{end}' 2>/dev/null || true)
  case "$msg" in
    True*) return 0 ;;
    *Unauthorized*)
      fail "the Azure DevOps seam is refusing to authenticate:
    $msg
  That is the credential, not the tools. Re-run — the driver refreshes it —
  or capture one by hand: make ado-secret ADO_ORG=$ado_org" ;;
    "") note "the Azure DevOps seam is not deployed; skipping its health check" ;;
    *) fail "the Azure DevOps seam is not accepted: $msg" ;;
  esac
}
step()  { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }
note()  { printf '   %s\n' "$*" >&2; }
fail()  { printf '\nrelease-run: %s\n' "$*" >&2; exit 1; }

# One agent turn. The reply and the tool calls are printed — a person
# watching a release wants to see what it did, not a task object — and
# the raw output is kept for the caller to read back.
turn() {
  local out=$1 task=$2
  # TASK travels as a make variable into a shell recipe ("$(TASK)"), so a
  # double quote or a newline in it breaks the RECIPE — the failure looks
  # like `/bin/sh: Syntax error: Unterminated quoted string`, nowhere near
  # the prompt that caused it. Refuse both here rather than debug that
  # twice. (Found the hard way on the first real run.)
  # $'\n' and not "$(printf '\n')": command substitution strips trailing
  # newlines, so the latter is an EMPTY pattern that matches every task.
  case "$task" in
    *'"'*)   fail "internal: a task carries a double quote: $task" ;;
    *$'\n'*) fail "internal: a task carries a newline: $task" ;;
  esac
  # shellcheck disable=SC2086 # RELEASE_CHAT is a command line, not a word
  local chat_rc=0
  $RELEASE_CHAT TASK="$task" > "$out" 2>&1 || chat_rc=$?
  python3 "$here/show-turn.py" "$out" >&2 || tail -30 "$out" >&2
  # The turn is only a turn if the task COMPLETED with a reply. Anything
  # else is a failure the caller must see, because everything downstream
  # reads the answer. This function used to print a note and return 0,
  # which made a failed turn look like a successful one to every caller —
  # the exact thing the agent's own instructions forbid, committed in the
  # driver that enforces them.
  if [ "$chat_rc" != 0 ]; then
    note "the agent's turn exited $chat_rc"
    return 1
  fi
  python3 "$here/verify-chat.py" "$out" >/dev/null 2>&1 || {
    note "the agent's turn did not complete with a reply"
    return 1
  }
}

# ---------------------------------------------------------------------
# The consequential calls, named once, as the arguments the agent is
# asked to make and the summary the plane will file them under.
#
# The summary strings below must name EVERY policy-relevant field the
# committed table declares for that tool (k8s/plane/upstreams.yaml), in
# the declared order, because that is exactly what the audit's arg_summary
# renders and it is how this script tells the intended call apart from a
# different one. A selector less specific than the digest can match a
# request a human then approves by mistake.
# ---------------------------------------------------------------------

# request_id <tool> <summary substring> -> the pending request for THAT
# call, or empty. Never selected by position.
request_id() {
  admin approvals > "$work/approvals.out"
  awk -v cred="$CRED_RELEASE" -v tool="$1" -v want="$2" \
    '$3==cred && $4=="tool" && $5==tool && index($0, want) {print $1; exit}' "$work/approvals.out"
}

# consequential <tool> <args-json> <expected summary> <task text>
#
# File the request for the exact call, wait for a human to approve it,
# then have the AGENT make it, and require the plane's own audit to show
# it admitted under the grant.
#
# WHY THE DRIVER FILES THE REQUEST AND NOT THE AGENT. The first real run
# tried the obvious thing — ask the agent, let the gateway deny it, let
# the denial file the request — and the agent answered "there is no
# create_branch tool available in my toolset". It was right. The gateway
# PROJECTS tools/list down to what a credential may call: the allowlist,
# plus tools it holds a live grant for, plus tools it carries a standing
# constraint for (gateway.go, projectable). A consequential tool is none
# of those by design, so kagent wires the agent no such tool, so the
# agent can never attempt the call, so no denial and no request can ever
# happen. Deny-and-pend only closes the loop for a tool the agent can
# already SEE.
#
# So the order inverts, and it is better for it. The driver files the
# request naming the exact call — `plane-admin.sh request` exists for
# this, and computes the digest exactly as the gateway will, so the
# request and the agent's later call are provably the same call. A human
# approves it. The grant then makes the tool projectable, the agent can
# see it for the first time, and it makes the call under a grant welded
# to those arguments. A different call is denied even now.
#
# What that buys, said plainly: a human can only ever be shown the call
# the operator asked for, because the driver is what wrote it down. The
# earlier design could show them whatever the model happened to emit.
consequential() {
  local tool=$1 args=$2 want=$3 ask=$4

  step "Proposing: $tool — $want"

  # An existing live grant would make the agent's call succeed without
  # anyone approving anything in THIS run. Refuse rather than ride it.
  admin grants "$CRED_RELEASE" > "$work/$tool.grants-before.out" 2>/dev/null || true
  if awk -v cred="$CRED_RELEASE" -v subj="$tool" \
      '$2==cred && $3=="tool" && $4==subj && $5=="yes" {found=1} END{exit !found}' \
      "$work/$tool.grants-before.out"; then
    cat "$work/$tool.grants-before.out" >&2
    fail "$tool already carries a LIVE grant on $CRED_RELEASE. This run would
  spend an approval somebody gave earlier, for a call this run did not
  describe. Let it lapse, or deny it, and start again."
  fi

  admin request "$CRED_RELEASE" tool "$tool" "$args" >&2

  local id
  id=$(request_id "$tool" "$want")
  if [ -z "$id" ]; then
    admin approvals >&2 || true
    fail "the request for this call was not filed:
    $want
  Nothing has been approved and nothing was done."
  fi
  note "Filed as request $id. What a human is asked is the CALL:"
  grep -F "$id" "$work/approvals.out" >&2

  CRED="$CRED_RELEASE" HUMAN_TIMEOUT="${HUMAN_TIMEOUT:-900}" \
    ADMIN_PORT="$ADMIN_PORT" bash "$here/await-approval.sh" "$id" "${SLACK_USER:--}" 1

  step "Approved — the agent makes the call"
  turn "$work/$tool.do.out" "$ask" || note "the agent's turn did not complete cleanly"

  admin tool-audit "$CRED_RELEASE" > "$work/$tool.audit.out"
  grep -E "$CRED_RELEASE +[a-z-]+ +tools/call +$tool +allowed +2[0-9][0-9] +granted " \
    "$work/$tool.audit.out" > /dev/null \
    || { cat "$work/$tool.audit.out" >&2
         fail "$tool was approved but the call was not admitted under the grant.
  Nothing is being reported as done that the plane did not record."; }
  note "Admitted under the grant. The filed request and this row carry the same"
  note "digest: the call a human approved is provably the call that ran."
}

# --- 1. propose -------------------------------------------------------
do_propose() {
  step "What shipped last, and what merged since — on $repo"
  turn "$work/propose.out" \
"For the GitHub repository with owner $owner and repo $name, do three things in order. \
First, call get_latest_release and list_tags, and report the previous release tag and its date. \
Second, call list_pull_requests with state closed, base $base, sort updated, direction desc, perPage 50, \
and fields set to exactly number, title and merged_at -- do NOT ask for the body field, the listing does not \
fit in your context with it. A closed pull request is not necessarily merged, so count only those whose \
merged_at field is set and is later than the previous release; if every one of the 50 merged after it, \
say so and ask for the next page rather than guessing. \
Third, draft release notes for $version as short user-facing bullets grouped by theme, working from the \
titles alone, and list separately any pull request whose title says nothing a user could act on, with its \
number and title -- do not invent a meaning for it. \
Do not create anything. Report only what the tools told you." \
    || fail "the agent could not complete the proposal turn"
  python3 "$here/show-turn.py" "$work/propose.out" --reply-only > "$work/notes.txt" || true
  note "Draft notes saved. Nothing has been created."
}

# --- 2. cut the release branch ---------------------------------------
do_cut() {
  consequential create_branch \
    "{\"owner\": \"$owner\", \"repo\": \"$name\", \"branch\": \"$branch\", \"from_branch\": \"$base\"}" \
    "create_branch: owner $owner, repo $name, branch $branch, from_branch $base" \
"Call create_branch with owner $owner, repo $name, branch $branch, from_branch $base. Make exactly that one call and report what the tool returned."
}

# --- 3. start the builds ---------------------------------------------
do_build() {
  local IFS=,
  for wf in $gh_workflows; do
    [ -n "$wf" ] || continue
    consequential actions_run_trigger \
      "{\"method\": \"run_workflow\", \"owner\": \"$owner\", \"repo\": \"$name\", \"workflow_id\": \"$wf\", \"ref\": \"$branch\"}" \
      "actions_run_trigger: method run_workflow, owner $owner, repo $name, workflow_id $wf, ref $branch" \
"Call actions_run_trigger with method run_workflow, owner $owner, repo $name, workflow_id $wf, ref $branch. Make exactly that one call and report what the tool returned."
  done
  refresh_ado
  for pid in $ado_pipelines; do
    [ -n "$pid" ] || continue
    consequential pipelines_write \
      "{\"action\": \"run_pipeline\", \"orgName\": \"$ado_org\", \"project\": \"$ado_project\", \"pipelineId\": $pid}" \
      "pipelines_write: action run_pipeline, orgName $ado_org, project $ado_project, pipelineId $pid" \
"Call pipelines_write with action run_pipeline, orgName $ado_org, project $ado_project, pipelineId $pid. Make exactly that one call and report what the tool returned."
  done
}

# --- 4. publish ------------------------------------------------------
#
# The decision is governed the same way every other consequential step
# is: a request naming the exact release, a human, a grant welded to it.
# The TRANSFER is the driver's, not the gateway's — see
# scripts/release-publish.sh for why, and for what that does and does not
# buy.
do_publish() {
  [ -n "$ado_builds" ] || fail "publishing needs ADO_BUILDS (the build ids whose artifacts to attach)"
  [ -s "$work/notes.txt" ] || fail "no drafted notes to publish — run STEP=propose first, or pass NOTES_FILE"

  local want="release_publish: owner $owner, repo $name, tag $version"
  step "Proposing: the release itself — $want"

  admin grants "$CRED_RELEASE" > "$work/pub.grants-before.out" 2>/dev/null || true
  if awk -v cred="$CRED_RELEASE" '$2==cred && $3=="tool" && $4=="release_publish" && $5=="yes" {f=1} END{exit !f}' \
      "$work/pub.grants-before.out"; then
    fail "a live grant for release_publish already exists on $CRED_RELEASE. This run
  would spend an approval given earlier, for a release this run did not
  describe. Let it lapse, or deny it, and start again."
  fi

  # The asset list BEFORE the approval, so the person deciding sees what
  # lands on a public release rather than a count they have to trust.
  step "What would be attached to $version"
  GITHUB_REPO="$repo" VERSION="$version" NOTES_FILE="$work/notes.txt" \
    ADO_ORG="$ado_org" ADO_PROJECT="$ado_project" ADO_BUILDS="$ado_builds" \
    ADO_ARTIFACTS="${ADO_ARTIFACTS:-}" ASSET_GLOBS="${ASSET_GLOBS:-}" \
    bash "$here/release-publish.sh" --list \
    || fail "could not read the builds' artifacts — nothing was created"

  admin request "$CRED_RELEASE" tool release_publish \
    "{\"owner\": \"$owner\", \"repo\": \"$name\", \"tag\": \"$version\"}" >&2
  local id
  id=$(request_id release_publish "$want")
  [ -n "$id" ] || { admin approvals >&2 || true; fail "the publish request was not filed"; }
  note "Filed as request $id. The notes a human is approving are in $work/notes.txt:"
  head -20 "$work/notes.txt" >&2

  CRED="$CRED_RELEASE" HUMAN_TIMEOUT="${HUMAN_TIMEOUT:-900}" \
    ADMIN_PORT="$ADMIN_PORT" bash "$here/await-approval.sh" "$id" "${SLACK_USER:--}" 1

  step "Approved — publishing"
  GITHUB_REPO="$repo" VERSION="$version" NOTES_FILE="$work/notes.txt" \
    ADO_ORG="$ado_org" ADO_PROJECT="$ado_project" ADO_BUILDS="$ado_builds" \
    ADO_ARTIFACTS="${ADO_ARTIFACTS:-}" ASSET_GLOBS="${ASSET_GLOBS:-}" \
    RELEASE_TARGET="$branch" PRERELEASE="${PRERELEASE:-1}" \
    bash "$here/release-publish.sh"
}

# --- 5. watch --------------------------------------------------------
#
# The POLLING lives here, in shell, and not inside an agent turn. A turn
# is request/response and a build is minutes; a model re-deciding "has it
# finished yet" every minute would burn budget on a question the status
# tool answers exactly, and would put an unbounded wait inside a bounded
# turn. The P7b inbound bridge could deliver a completion callback
# instead, and was rejected for this: it needs a public HTTPS edge that
# exists on AKS only, and on its generic hooks the webhook body becomes
# the agent's prompt verbatim.
do_watch() {
  [ -n "$gh_workflows$ado_pipelines" ] || { note "nothing to watch"; return 0; }
  refresh_ado
  step "Watching the builds (up to ${WATCH_TIMEOUT}s, every ${WATCH_POLL}s)"
  local deadline=$(( $(date +%s) + WATCH_TIMEOUT )) n=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    n=$((n + 1))
    local ask="Report the current state of the release builds, from the tools only."
    if [ -n "$gh_workflows" ]; then
      ask="$ask For GitHub, call actions_list with method list_workflow_runs, owner $owner, repo $name, and report the runs on branch $branch with their status and conclusion."
    fi
    if [ -n "$ado_pipelines" ]; then
      ask="$ask For Azure DevOps, call pipelines_build with action list, orgName $ado_org, project $ado_project, and report the most recent builds with their status and result."
    fi
    ask="$ask End your answer with exactly one line reading STATE: RUNNING if anything is still in progress, or STATE: DONE if everything finished successfully, or STATE: FAILED if anything failed."
    turn "$work/watch.$n.out" "$ask" || true
    if grep -q '^STATE: DONE' "$work/watch.$n.out"; then
      note "The agent reports every build finished successfully."
      return 0
    fi
    if grep -q '^STATE: FAILED' "$work/watch.$n.out"; then
      step "A build failed — the agent reads the log"
      turn "$work/watch.fail.out" \
"A build failed. Read the failure. For Azure DevOps, call pipelines_build with action get_status and then \
pipelines_build_log for the failing build in orgName $ado_org project $ado_project. For GitHub, call actions_get with method \
get_workflow_run. Say what failed and quote what the log says. Do not guess at a cause."
      fail "a build failed — see above. Nothing further was done."
    fi
    sleep "$WATCH_POLL"
  done
  fail "the builds were still running after ${WATCH_TIMEOUT}s. Not claiming a
  result the tools did not report; re-run with STEP=watch to keep waiting."
}

# --- preflight -------------------------------------------------------
step "Release $version of $repo, from $base, on branch $branch"
if [ "$DRY_RUN" = 1 ]; then
  note "DRY_RUN=1 — the agent will read and draft, and stop before the first"
  note "consequential call. Nothing will be created."
fi
for secret in kaimahi-release-pat; do
  $KUBECTL -n kaimahi get secret "$secret" >/dev/null 2>&1 \
    || fail "Secret kaimahi/$secret is missing — run: make release-secret GITHUB_REPO=$repo"
done
if [ -n "$ado_pipelines" ] && ! $KUBECTL -n kaimahi get secret kaimahi-ado-token >/dev/null 2>&1; then
  fail "Azure DevOps pipelines were asked for, but Secret kaimahi/kaimahi-ado-token is missing.
  Run: make ado-secret ADO_ORG=<organization>   (that token lives about an hour)"
fi

# A fresh credential before any of it: the expiry is shorter than the
# process, so refreshing once at the start is not enough either — every
# ADO-touching step re-checks below.
refresh_ado
sleep "${ADO_PROJECTION_WAIT:-8}"
ado_seam_ok

case "$STEP" in
  propose) do_propose ;;
  cut)     do_cut ;;
  build)   do_build ;;
  watch)   do_watch ;;
  publish) do_propose; do_publish ;;
  all)
    do_propose
    if [ "$DRY_RUN" = 1 ]; then
      step "DRY_RUN — stopping before the first consequential call"
      exit 0
    fi
    do_cut
    do_build
    do_watch
    [ -z "$ado_builds" ] || do_publish
    ;;
  *) fail "unknown STEP '$STEP' (want propose, cut, build, watch, publish or all)" ;;
esac

step "The record: every decision this credential got"
admin tool-audit "$CRED_RELEASE" >&2
step "Who approved what, and which call"
admin approval-audit "$CRED_RELEASE" >&2

printf '\n\033[1mrelease-run: done. Every consequential call was denied first, approved by a\n' >&2
printf 'human against the exact call, and admitted under a grant welded to it.\033[0m\n' >&2
