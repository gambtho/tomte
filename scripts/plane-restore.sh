#!/usr/bin/env bash
# Restore a `make backup` file into the running plane's database (P9),
# REPLACING its contents: the dump drops and recreates every table.
#
# psql runs INSIDE the Postgres pod over its unix socket and reads the
# file from stdin through kubectl exec -i — the same custody as the
# backup (no password leaves the pod, nothing lands on disk in the
# cluster, no local client). ON_ERROR_STOP makes a partial restore a
# failure rather than a silently half-restored ledger; the whole file
# runs in one transaction so a failure leaves the database as it was.
#
# The proxies keep running: their next query sees the restored tables.
# A fresh cluster's migrations already created the schema, and the dump
# carries goose's version table, so no restart or re-migration is
# needed — and the restored token hashes mean the agent-side Secrets
# from the backed-up cluster work again.
#
# Usage: KUBECTL="kubectl --context ..." FILE=path plane-restore.sh
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE=kaimahi
FILE="${FILE:?FILE=<backup.sql> is required}"
test -s "$FILE" || { echo "plane-restore: $FILE is missing or empty" >&2; exit 1; }
grep -q '^-- PostgreSQL database dump complete' "$FILE" || {
  echo "plane-restore: $FILE is not a complete pg_dump (no trailer); refusing" >&2
  exit 1
}

# shellcheck disable=SC2086 # KUBECTL deliberately carries --context args
$KUBECTL -n "$NAMESPACE" exec -i deploy/kaimahi-postgres -- \
  psql -U kaimahi -d kaimahi -q -v ON_ERROR_STOP=1 --single-transaction < "$FILE"
# Well-formed positive: the ledger table answers a count after the restore.
n=$($KUBECTL -n "$NAMESPACE" exec deploy/kaimahi-postgres -- \
  psql -U kaimahi -d kaimahi -tAc 'SELECT count(*) FROM ledger_entry')
case "$n" in
  ''|*[!0-9]*) echo "plane-restore: ledger unreadable after restore: '$n'" >&2; exit 1 ;;
esac
echo "plane-restore: restored $FILE; ledger_entry has $n rows"
