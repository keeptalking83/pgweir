#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""A simulated agent that writes to a pgweir branch.

It runs three transactions on the branch, each tagged with the same run and a
different tool call, the way an agent framework would tag the SQL issued by
each tool invocation:

  call_1  ships 20 orders (UPDATE)
  call_2  creates 5 orders (INSERT)
  call_3  removes 2 orders (DELETE)

Usage: agent_demo.py BRANCH_DSN
Requires psycopg 3 (pip install "psycopg[binary]").
"""
import json
import sys
import uuid

import psycopg


def tag(cur, run_id, tool_call_id):
    """Tag the current transaction; must run inside it, before the writes."""
    cur.execute(
        "SELECT pgweir.tag(%s::jsonb)",
        (json.dumps({"run_id": run_id, "tool_call_id": tool_call_id, "agent": "inventory-bot"}),),
    )


def main():
    if len(sys.argv) != 2:
        print("usage: agent_demo.py BRANCH_DSN", file=sys.stderr)
        sys.exit(2)
    run_id = "run_" + uuid.uuid4().hex[:8]
    with psycopg.connect(sys.argv[1], autocommit=True) as conn:
        with conn.transaction(), conn.cursor() as cur:
            tag(cur, run_id, "call_1")
            cur.execute("UPDATE orders SET status = 'SHIPPED' WHERE id BETWEEN 1 AND 20")
            shipped = cur.rowcount
        with conn.transaction(), conn.cursor() as cur:
            tag(cur, run_id, "call_2")
            cur.execute(
                "INSERT INTO orders (customer_id, status, amount, note) "
                "SELECT g, 'NEW', 25 * g, 'created by agent' FROM generate_series(1, 5) AS g"
            )
            created = cur.rowcount
        with conn.transaction(), conn.cursor() as cur:
            tag(cur, run_id, "call_3")
            cur.execute("DELETE FROM orders WHERE id IN (999, 1000)")
            removed = cur.rowcount
    print(f"agent {run_id}: shipped {shipped}, created {created}, removed {removed} orders", file=sys.stderr)


if __name__ == "__main__":
    main()
