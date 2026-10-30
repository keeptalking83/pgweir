#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end pgweir demo, the same commands as the README quickstart:
# branch -> agent writes -> concurrent writes on main -> diff ->
# merge --policy fail (conflict) -> merge --policy skip -> prove -> verify.
#
# Needs a fresh database: docker compose -f examples/docker-compose.yml up -d --wait
# (reset with "down -v"), pgweir on PATH, psql, and Python 3 with psycopg 3.
# Key, public key and proof are written to this directory.
set -euo pipefail
cd "$(dirname "$0")"

export PGWEIR_MAIN_DSN="${PGWEIR_MAIN_DSN:-postgres://app:app@localhost:55432/shop?sslmode=disable}"
export PGWEIR_SIGNING_KEY=pgweir.key
MAIN="$PGWEIR_MAIN_DSN"
rm -f pgweir.key pgweir.pub proof.json

# run prints a command, runs it in this shell and returns its status.
# DEMO_PAUSE (seconds, default 0) slows the demo down for recordings.
run() {
  printf '\n\033[1m$ %s\033[0m\n' "$1"
  local status=0
  eval "$1" || status=$?
  sleep "${DEMO_PAUSE:-0}"
  return "$status"
}

run 'pgweir init --tables "public.customers,public.orders"'
run 'pgweir keygen --out pgweir.key > pgweir.pub'
run 'DSN=$(pgweir branch create demo --provider local)'
run 'python3 agent_demo.py "$DSN"'
run 'psql "$MAIN" -c "UPDATE orders SET note = '"'touched on main'"' WHERE id = 5"'    # disjoint cell: merges cleanly
run 'psql "$MAIN" -c "UPDATE orders SET status = '"'CANCELLED'"' WHERE id = 7"'       # same cell: CELL_CONFLICT
run 'pgweir diff demo'

set +e
run 'pgweir merge demo --policy fail'                                                  # exit 5: conflict on orders id=7
code=$?
set -e
if [ "$code" -ne 5 ]; then
  echo "expected exit code 5 from merge --policy fail, got $code" >&2
  exit 1
fi

run 'MERGE_ID=$(pgweir merge demo --policy skip --yes)'                                # applies everything else
run 'psql "$MAIN" -c "INSERT INTO orders (customer_id, status, amount) VALUES (1, '"'NEW'"', 10)"'
run 'pgweir prove "$MERGE_ID" --out proof.json'
run 'pgweir verify --proof proof.json --pubkey pgweir.pub'
