# pgweir

[![ci](https://github.com/keeptalking83/pgweir/actions/workflows/ci.yml/badge.svg)](https://github.com/keeptalking83/pgweir/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**A controlled write-and-merge layer between AI agents and your production
Postgres.**

pgweir lets an agent write to a branch (a fork of your database), merges the
rows it changed back into main with a three-way merge under a conflict policy
you choose, and records every merge in a signed, tamper-evident ledger.

![pgweir demo: branch, agent writes, conflicting edit on main, diff, merge with fail then skip, prove and verify](docs/demo.gif)

A weir is a low dam that lets water through under control. pgweir is a
command-line sidecar: it does not replace Postgres, needs no extension and no
superuser, and works on vanilla and managed Postgres 14 to 18.

## Why

Agents that do real work need to write to a database. Giving them main is
risky, and replaying their SQL later is fragile. Forking is easy (Neon
branches, `CREATE DATABASE ... TEMPLATE`, snapshots), but getting the changes
back is not: main kept moving while the agent worked.

pgweir closes that loop:

1. **Fork.** `pgweir branch create` forks main. The agent connects to the fork
   with a role of its own and never touches main.
2. **Capture.** Every row the agent changes is read from the branch's
   write-ahead log through logical replication, with the agent run that made
   it.
3. **Merge.** Each changed row is compared three ways: at fork time, on the
   branch, and on main now. Changes that do not collide are applied in one
   transaction; collisions are conflicts, handled by `fail`, `skip` or
   `accept`.
4. **Prove.** Each merge is a leaf of an RFC 6962 Merkle tree with a signed
   tree head. Anyone with the public key can verify a merge offline.

## Quickstart

You need Docker, Go 1.25 or later, `psql`, and Python 3. Five minutes, start
to finish. Without Go, download the `pgweir` binary for Linux or macOS from
[Releases](https://github.com/keeptalking83/pgweir/releases), put it on your
`PATH`, and skip the `go install` line. The binaries are not signed: on macOS,
one downloaded with a browser is blocked by Gatekeeper until you run
`xattr -d com.apple.quarantine pgweir`.

```sh
git clone https://github.com/keeptalking83/pgweir && cd pgweir
go install ./cmd/pgweir                 # installs to $(go env GOPATH)/bin; put it on PATH
docker compose -f examples/docker-compose.yml up -d --wait
pip install "psycopg[binary]"           # for the simulated agent
cd examples
```

The container is Postgres 16 with `wal_level = logical` and a small shop:
100 customers and 1,000 orders, owned by the role `app`, which is not a
superuser. pgweir runs as `app`. The agent gets a role of its own, `agent`,
which has no privileges on main.

```sh
export PGWEIR_MAIN_DSN='postgres://app:app@localhost:55432/shop?sslmode=disable'
export PGWEIR_SIGNING_KEY=pgweir.key
MAIN=$PGWEIR_MAIN_DSN

pgweir init --tables "public.customers,public.orders"
pgweir keygen --out pgweir.key > pgweir.pub
DSN=$(pgweir branch create demo --provider local)
```

`$DSN` uses pgweir's own role. Give the agent privileges on the fork only,
and its own connection string:

```sh
psql "$DSN" -q -f grant_agent.sql                                            # grants exist on the fork only
AGENT_DSN="postgres://agent:agent@localhost:55432/pw_demo?sslmode=disable"   # the fork is database pw_demo
```

The agent now works on the branch: it ships 20 orders, creates 5, and deletes
2, in three tagged transactions. With the same role, main is out of reach:

```console
$ python3 agent_demo.py "$AGENT_DSN"
agent run_4c9047f3: shipped 20, created 5, removed 2 orders
$ psql "postgres://agent:agent@localhost:55432/shop?sslmode=disable" -c "UPDATE orders SET status = 'HACKED'"
ERROR:  permission denied for table orders
```

Meanwhile someone edits main:

```sh
psql "$MAIN" -c "UPDATE orders SET note = 'touched on main' WHERE id = 5"    # another column than the agent's: merges cleanly
psql "$MAIN" -c "UPDATE orders SET status = 'CANCELLED' WHERE id = 7"       # the column the agent changed: conflict
```

See what the agent did:

```console
$ pgweir diff demo
branch demo: 27 rows changed (base 0/1E25260, end 0/1E271F8)

public.orders  inserted 5  updated 20  deleted 2
  id=1 | U | status: PENDING -> SHIPPED
  id=2 | U | status: PENDING -> SHIPPED
  ...
  id=999 | D | deleted
  id=1000 | D | deleted
  id=1001 | I | amount=25.00, customer_id=1, note=created by agent, status=NEW
  ...
```

Merge with `fail` first: any conflict stops the merge, nothing is applied.

```console
$ pgweir merge demo --policy fail
merge 3714caf0-... of branch "demo", policy FAIL: 27 changed rows: 5 to insert, 19 to update, 2 to delete, 1 skipped, 1 conflict
  public.orders                  insert 5  update 19  delete 2  conflicts 1
conflicts:
  public.orders id="7" CELL_CONFLICT (status: base "PENDING", main "CANCELLED", branch "SHIPPED") [UNRESOLVED]
3714caf0-...
error: CONFLICTS: 1 conflict under policy fail; nothing was applied (see `pgweir conflicts 3714caf0-...`)
```

The exit code is 5. Keep main's version of order 7 and apply everything else:

```console
$ MERGE_ID=$(pgweir merge demo --policy skip --yes)
...
applied merge 0bfe5bb6-...: leaf 0, tree size 1, root 838ced6f3b0bddbd...
branch "demo" is now MERGED; prove the merge with: pgweir prove 0bfe5bb6-... --out proof.json
```

Order 5 now has the agent's status and main's note. The agent's new orders
are on main, and main's identity sequence was moved past their keys, so main
keeps inserting without collisions:

```sh
psql "$MAIN" -c "INSERT INTO orders (customer_id, status, amount) VALUES (1, 'NEW', 10)"
```

Prove the merge, then verify the proof offline with nothing but the public
key:

```console
$ pgweir prove "$MERGE_ID" --out proof.json
$ pgweir verify --proof proof.json --pubkey pgweir.pub
valid: merge 0bfe5bb6-... is leaf 0 of the tree of size 1 signed by key f24357979314a688 at 2026-10-30T08:57:44.479Z
```

[`examples/conflict_demo.sh`](examples/conflict_demo.sh) runs exactly these
commands; CI runs it on every change. Reset the demo with
`docker compose -f examples/docker-compose.yml down -v`.

## How it works

```mermaid
flowchart LR
    agent([Agent]) -- "SQL + pgweir.tag()" --> branch[(Branch)]
    branch -- "logical replication" --> capture[capture]
    capture --> events[(events on main)]
    events --> engine[three-way merge]
    main[(Main)] -- "current rows" --> engine
    engine --> apply[apply: one transaction]
    apply --> main
    apply --> ledger[(signed ledger)]
```

- **Branch creation** sets `REPLICA IDENTITY FULL` and a publication on the
  fork, then creates a replication slot, and only then reveals the connection
  string. Every change is captured with its full old row, so the first old row
  of each key is its value at fork time. Main's tables are never altered.
- **Capture** streams the slot with pgoutput. A change is stored on main
  before the slot is acknowledged, so a crash loses nothing and duplicates
  nothing.
- **Merge** compares rows by their Postgres text representation, under pinned
  session settings, so time zones and date styles cannot create false
  conflicts.
- **Apply** locks every affected main row, checks that it still matches what
  the merge was computed from, and applies inserts, updates and deletes in
  foreign-key order in one transaction. If main changed meanwhile, it rolls
  back and asks you to rerun (exit 3).

[docs/design.md](docs/design.md) has the details and the reasoning behind
each step.

## Merge rules

For each row: B is its value at fork time, T on the branch, O on main now.

| Branch did | Main did | Result |
|---|---|---|
| insert | nothing | insert |
| insert | inserted the same key, different row | conflict `INSERT_INSERT` |
| update | nothing | update |
| update | changed other columns | merged row: main's row with the branch's columns |
| update | changed the same column differently | conflict `CELL_CONFLICT` |
| update | deleted the row | conflict `MODIFY_DELETE` |
| delete | nothing | delete |
| delete | changed the row | conflict `DELETE_MODIFY` |

| Policy | Conflicting rows |
|---|---|
| `fail` | nothing is applied (exit 5) |
| `skip` | stay as they are on main; the rest is applied |
| `accept` | take the branch's version |

`pgweir merge --dry-run` shows the plan without applying it, `--out plan.json`
writes it as canonical JSON, and `pgweir conflicts <merge-id>` lists the
conflicts of any merge, including failed ones.

## Tagging agent writes

The agent tags each transaction with the run it belongs to:

```sql
BEGIN;
SELECT pgweir.tag('{"run_id":"run_7f3a","parent_run_id":"run_1b00","tool_call_id":"call_42","agent":"inventory-bot"}');
UPDATE orders SET status = 'SHIPPED' WHERE id = 42;
COMMIT;
```

Runs appear in the changeset (`pgweir diff --json`, `--run <run_id>`) and in
the signed ledger record of every merge. Untagged writes are merged too, as
run `untagged`. See [docs/agent-protocol.md](docs/agent-protocol.md) for the
rules and snippets for psycopg, SQLAlchemy, pgx and node-postgres.

## Works with

| Platform | Provider | Prerequisites |
|---|---|---|
| Postgres 14–18, self-hosted or Docker | `local` | `wal_level = logical` (restart); a role that owns the database and tables, with `REPLICATION` and `CREATEDB` |
| Neon | `neon` | logical replication enabled on the project; a role created by Neon (it has `REPLICATION`); `NEON_API_KEY`, `NEON_PROJECT_ID` |
| Amazon RDS / Aurora PostgreSQL | `manual` | `rds.logical_replication = 1` (reboot); `GRANT rds_replication TO <role>`; fork with a snapshot restore or an Aurora clone |
| Supabase | `manual` | Supabase Branching does not copy data, so fork by restoring a backup into another project or database |
| Any other Postgres 14+ | `manual` | logical replication on the fork's server; a role with `REPLICATION` |

On every platform, tracked tables need a primary key, and the role that runs
pgweir must own them. The fork's server needs `max_replication_slots` and
`max_wal_senders` of at least the number of open branches. `pgweir check`
reports what is missing. With `manual`, you create the fork and pass it with
`--branch-dsn`; pgweir cannot verify that nothing wrote to it before the
branch was created. Details and caveats per platform:
[docs/providers.md](docs/providers.md).

## Commands

| Command | Does |
|---|---|
| `pgweir check` | report server, role and table prerequisites; changes nothing |
| `pgweir init --tables a,b` (or `--all-tables --schemas s`) | install the `pgweir` schema on main and choose tracked tables |
| `pgweir branch create <name> --provider local\|neon\|manual` | fork main; prints the branch connection string on stdout |
| `pgweir branch list` / `branch drop <name> [--force]` | list branches; drop slot and fork |
| `pgweir diff <name> [--json] [--out f] [--table t] [--run r]` | capture and show the changeset |
| `pgweir merge <name> --policy fail\|skip\|accept [--dry-run] [--yes] [--out f]` | merge; prints the merge id on stdout |
| `pgweir conflicts <merge-id>` | list a merge's conflicts |
| `pgweir keygen [--out f]` | create an ed25519 signing key; prints the public key |
| `pgweir prove <merge-id> [--out f]` | inclusion proof against the latest signed tree head |
| `pgweir verify --proof f --pubkey k` | verify a proof offline |
| `pgweir ledger sth \| consistency \| verify-chain \| export` | inspect, check and export the ledger |

Global flags: `--dsn` (`PGWEIR_MAIN_DSN`), `--signing-key`
(`PGWEIR_SIGNING_KEY`), `--idle-timeout`, `--log-level`, `--json`. Machine
output goes to stdout, everything else to stderr.

Exit codes of `merge`: 0 applied (or dry run), 2 needs `--yes`, 3 main changed
meanwhile (rerun), 4 rejected by Postgres (rolled back), 5 conflicts under
`fail`.

## The ledger

Each applied merge is recorded with its changeset hash, policy, statistics,
conflicts hash, agent runs and LSNs as a leaf of an RFC 6962 Merkle tree. Each
tree size gets a tree head signed with ed25519; the private key never touches
the database.

The ledger is **tamper-evident, not tamper-proof.** Someone with write access
to main can rewrite it, but cannot make the result agree with proofs and tree
heads published earlier. Export the ledger regularly to a place they cannot
change, such as an S3 bucket with Object Lock:

```sh
pgweir ledger export --s3 s3://audit-bucket/pgweir --include-changesets
pgweir ledger verify-chain --sths ./saved-export --pubkey pgweir.pub
```

Formats and the verification procedure: [docs/ledger.md](docs/ledger.md).

## Limitations

pgweir v0.1 is deliberately narrow. It merges row changes, not schema changes
(schema drift aborts the merge). It needs a primary key on every tracked
table. A branch merges once. Sequences, large objects, partitioned and
unlogged tables are not supported. The full list is in
[NON-GOALS.md](NON-GOALS.md).

Tables whose keys come from a sequence work, but rows inserted concurrently on
main and on a branch can draw the same key. `init` warns about such tables;
prefer UUID keys where main and agents both insert.

## Security

Main is the trusted store: it holds branch connection strings and the ledger.
The connection string `branch create` prints uses pgweir's own role, which
can also reach main. Give agents a role of their own with privileges on the
fork only, as the quickstart does with
[`examples/grant_agent.sql`](examples/grant_agent.sql). The threat model, key
handling and agent isolation are described in [SECURITY.md](SECURITY.md).

## Status

v0.1. Tested on Postgres 14, 15, 16, 17 and 18. The Neon provider is tested
against a fake API; a recorded run against a real Neon project is still
pending (see [docs/providers.md](docs/providers.md)).

## Contributing

Issues and pull requests are welcome; please read
[CONTRIBUTING.md](CONTRIBUTING.md) first. Design decisions are logged in
[docs/DECISIONS.md](docs/DECISIONS.md).

## License

[Apache License 2.0](LICENSE). PostgreSQL is a trademark of the PostgreSQL
Community Association; pgweir is an independent project.
