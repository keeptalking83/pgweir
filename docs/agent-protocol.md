# Agent tagging protocol

pgweir records, for every row an agent changes, which agent run made the
change. The agent declares the run by calling `pgweir.tag()` inside each
transaction it runs on the branch. Untagged writes are still captured and
merged; they are attributed to the run `untagged`.

## The call

At the beginning of every transaction on the branch:

```sql
SELECT pgweir.tag('{"run_id":"run_7f3a","parent_run_id":"run_1b00","tool_call_id":"call_42","agent":"inventory-bot"}'::jsonb);
```

`pgweir.tag(jsonb)` emits a transactional logical decoding message with the
prefix `pgweir`. It writes nothing to any table.

## Rules

1. **Same transaction.** The tag applies only to the transaction it is called
   in. In autocommit mode every statement is its own transaction, so open an
   explicit transaction (`BEGIN ... COMMIT`) and call `pgweir.tag()` first.
2. **`run_id` is required**: a non-empty string. `parent_run_id`,
   `tool_call_id` and `agent` are optional and free-form; any other fields are
   kept as well.
3. **The last tag wins.** A second `pgweir.tag()` in the same transaction
   applies to the statements after it.
4. **Tags do not carry over.** The next transaction is `untagged` until it
   calls `pgweir.tag()` again.
5. **Malformed tags are rejected at once.** `pgweir.tag()` raises an error,
   inside the agent's own transaction, unless its argument is a JSON object
   with a non-empty string `run_id` and every number fits a 64-bit float.
   Tags are canonicalized with RFC 8785, so numbers are stored as IEEE 754
   doubles; send identifiers and large values as strings. (A `pgweir` message
   emitted without `pgweir.tag()` and failing these checks aborts capture with
   `INVALID_TAG`; the branch can then only be dropped.)

Run metadata appears in the changeset (`pgweir diff --json`, `"runs"`), and
the run objects of every merge are part of its signed ledger record. When the
same `run_id` is tagged with different metadata, the first tag captured wins.
`pgweir diff <branch> --run <run_id>` shows one run's rows.

Tags are claims made by the agent, not authenticated identities (see
[SECURITY.md](../SECURITY.md)).

## Privileges

If the agent connects with its own role (recommended, see
[SECURITY.md](../SECURITY.md)), grant on the fork after `branch create`:

```sql
GRANT USAGE ON SCHEMA pgweir TO agent;
GRANT EXECUTE ON FUNCTION pgweir.tag(jsonb) TO agent;
```

plus its usual privileges on the tracked tables. `pgweir.tag()` needs neither
`REPLICATION` nor superuser.

## Snippets

### Python, psycopg 3

```python
import json
import psycopg

def tag(cur, run_id, tool_call_id=None, parent_run_id=None, agent=None):
    cur.execute("SELECT pgweir.tag(%s::jsonb)", (json.dumps({
        "run_id": run_id, "tool_call_id": tool_call_id,
        "parent_run_id": parent_run_id, "agent": agent,
    }),))

with psycopg.connect(branch_dsn, autocommit=True) as conn:
    with conn.transaction(), conn.cursor() as cur:
        tag(cur, "run_7f3a", tool_call_id="call_42", agent="inventory-bot")
        cur.execute("UPDATE orders SET status = 'SHIPPED' WHERE id = %s", (42,))
```

### Python, SQLAlchemy 2

```python
import json
from sqlalchemy import create_engine, text

engine = create_engine(branch_url)  # postgresql+psycopg://...

with engine.begin() as conn:  # one transaction, committed at the end
    conn.execute(
        text("SELECT pgweir.tag(CAST(:t AS jsonb))"),
        {"t": json.dumps({"run_id": "run_7f3a", "tool_call_id": "call_42"})},
    )
    conn.execute(text("UPDATE orders SET status = 'SHIPPED' WHERE id = :id"), {"id": 42})
```

With an ORM `Session`, call `session.execute(text(...))` for the tag before
`session.flush()` or `session.commit()`; the tag and the flushed writes share
the session's transaction.

### Go, pgx v5

```go
tag, _ := json.Marshal(map[string]any{"run_id": "run_7f3a", "tool_call_id": "call_42"})

err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pgweir.tag($1::jsonb)", string(tag)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE orders SET status = 'SHIPPED' WHERE id = $1", 42)
	return err
})
```

### Node.js, node-postgres

```js
const client = await pool.connect();
try {
  await client.query("BEGIN");
  await client.query("SELECT pgweir.tag($1::jsonb)", [
    JSON.stringify({ run_id: "run_7f3a", tool_call_id: "call_42" }),
  ]);
  await client.query("UPDATE orders SET status = 'SHIPPED' WHERE id = $1", [42]);
  await client.query("COMMIT");
} catch (err) {
  await client.query("ROLLBACK");
  throw err;
} finally {
  client.release();
}
```

Use one client for the whole transaction; `pool.query()` may run each
statement on a different connection.

## Connection poolers

A tag is part of the agent's transaction, so it works through a transaction
pooler (PgBouncer in transaction mode, Neon's pooler) as long as the tag and
the writes are in the same transaction.
