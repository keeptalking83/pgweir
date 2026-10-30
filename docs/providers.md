# Providers

A provider creates the fork that becomes a branch, and destroys it when the
branch is dropped. Whatever the provider, `pgweir branch create` then does the
same on the fork: it checks the server settings and the tracked tables, sets
`REPLICA IDENTITY FULL` on the tracked tables, creates the `pgweir`
publication and the replication slot `pw_<name>`, and only then prints the
connection string.

Every provider needs:

| Requirement | Where | Why |
|---|---|---|
| Postgres 14 or later, same major version as main | fork | pgoutput `messages` option |
| `wal_level = logical` | fork's server | logical decoding |
| `max_replication_slots` and `max_wal_senders` ≥ open branches | fork's server | one slot and one walsender per capture |
| a role that owns the tracked tables and has `REPLICATION` | main and fork | capture, replica identity, publication |
| `CREATE` privilege on the database | fork | the publication |
| the `pgweir` schema | fork | `pgweir.tag()`; forks of main contain it |

`pgweir check` reports these for main's server.

## local

Forks main with `CREATE DATABASE "pw_<name>" TEMPLATE "<main>"` on the same
server. Use it for development, demos and CI.

- The role needs `CREATEDB` and must own main's database: Postgres lets only
  the owner (or a superuser) copy a database that is not marked as a template.
- No other session may be connected to main while it is copied. pgweir runs
  `CREATE DATABASE` from the maintenance database `postgres` (or
  `template1`), retries five times one second apart, then fails and lists the
  sessions that block it.
- The branch connection string is main's with the database name replaced.
- `branch drop` drops the slot, then `DROP DATABASE`, retried
  `WITH (FORCE)` if agents are still connected. pgweir only drops databases
  whose name starts with `pw_`.

```sh
pgweir branch create demo --provider local
```

## neon

Creates a [Neon](https://neon.com) branch with one read-write compute.

### Prerequisites

- **Logical replication enabled on the project.** Neon Console, project
  settings, or the API (`PATCH /projects/{project_id}` with
  `{"project":{"settings":{"enable_logical_replication":true}}}`). This sets
  `wal_level = logical` for every compute in the project, restarts them, and
  cannot be turned off again.
- **A role created by Neon** (Console, CLI or API). Those roles belong to
  `neon_superuser` and have `REPLICATION`. Roles created with SQL lack it and
  cannot be given it.
- `NEON_API_KEY` and `NEON_PROJECT_ID` in the environment.
- Main's connection string must be a **direct** (not pooled) connection for
  the role and database pgweir should use on the branch.

```sh
export NEON_API_KEY=... NEON_PROJECT_ID=...
pgweir branch create demo --provider neon                 # parent: project's default branch
pgweir branch create demo --provider neon --parent dev    # parent by name or br-... id
```

If main's host is a Neon endpoint (`ep-...`), the parent must be the branch
that endpoint belongs to; otherwise `branch create` stops and names the
`--parent` to use, because merging against the wrong base would be silently
wrong.

### Limits to know

- **Capture at least every 40 hours.** Neon removes replication slots that
  have not advanced for about 40 hours. Running `pgweir diff <branch>`
  advances the slot. If the slot is gone, uncaptured changes are lost and the
  branch becomes `FAILED`.
- A connection with an active walsender keeps the compute from scaling to
  zero while capture runs; between captures the compute may suspend.
- Replication connections cannot use Neon's pooler; pgweir always asks for a
  direct connection string.
- Projects allow 10 replication slots and 10 walsenders by default, which
  bounds the number of open branches.

### API calls

Verified against Neon's documentation and OpenAPI specification on
2026-10-30.

| Step | Request |
|---|---|
| list branches, find the parent (`default: true`, or by name/id) | `GET /projects/{project_id}/branches` |
| check main's endpoint belongs to the parent | `GET /projects/{project_id}/endpoints/{endpoint_id}` (`endpoint.branch_id`) |
| create the branch with a compute | `POST /projects/{project_id}/branches` with `{"branch":{"parent_id":"br-...","name":"pw_<name>"},"endpoints":[{"type":"read_write"}]}` |
| wait for each returned operation | `GET /projects/{project_id}/operations/{operation_id}` until `status` is `finished` or `skipped` (`failed`, `error`, `cancelled` abort) |
| get the connection string | `GET /projects/{project_id}/connection_uri?branch_id=&endpoint_id=&database_name=&role_name=&pooled=false` (`uri`) |
| delete on `branch drop` | `DELETE /projects/{project_id}/branches/{branch_id}` (`204` when already gone) |

Base URL `https://console.neon.tech/api/v2`, header
`Authorization: Bearer $NEON_API_KEY`. Responses `423` (operations still
running), `429` and `503` are retried with exponential backoff. After the
operations finish, pgweir retries connecting until the compute accepts
connections (up to five minutes).

Sources: <https://neon.com/docs/reference/api>,
<https://neon.com/api_spec/release/v2.json>,
<https://neon.com/docs/manage/branches>,
<https://neon.com/docs/manage/operations>,
<https://neon.com/docs/guides/logical-replication-neon>,
<https://neon.com/docs/guides/branching-intro>.

### Verification against a real Neon project

**Status: not yet run.** The provider is tested against a fake Neon API
(`internal/provider/neon_test.go`). Neon's documentation implies but does not
state that a replication slot can be created on a child branch's compute;
the manual run below confirms it. Record the output here when it has been
run.

```sh
export NEON_API_KEY=... NEON_PROJECT_ID=...
export PGWEIR_MAIN_DSN='postgresql://<role>:<password>@ep-....neon.tech/<db>?sslmode=require'
export PGWEIR_SIGNING_KEY=pgweir.key

# demo tables: the part of examples/seed.sql after the superuser setup
sed -n '/^CREATE TABLE customers/,$p' examples/seed.sql | psql "$PGWEIR_MAIN_DSN"
pgweir check
pgweir init --tables "public.customers,public.orders"
pgweir keygen --out pgweir.key > pgweir.pub
DSN=$(pgweir branch create neon_check --provider neon)
python3 examples/agent_demo.py "$DSN"
pgweir diff neon_check
MERGE_ID=$(pgweir merge neon_check --policy fail --yes)
pgweir prove "$MERGE_ID" --out proof.json
pgweir verify --proof proof.json --pubkey pgweir.pub
pgweir branch drop neon_check
```

Expected: the branch appears in the Neon Console as `pw_neon_check`, the merge
applies, `verify` prints `valid: ...`, and the Neon branch is deleted by
`branch drop`.

## manual

Uses a fork you created yourself, by any means, and passed with
`--branch-dsn`:

```sh
pgweir branch create audit_fix --provider manual --branch-dsn "$FORK_DSN"
```

- pgweir refuses a connection string that reaches main itself (it writes a
  marker to main and checks that the fork cannot see it).
- pgweir cannot verify that nothing was written to the fork between its
  creation and the replication slot. It prints a warning and records
  `provider_ref.warning = "unverified_base"`. Do not let anyone write to the
  fork until `branch create` has printed the connection string.
- The fork may come from a snapshot restore, a storage-level clone, or
  `pg_dump`/`pg_restore`; types are compared by name, so differing type OIDs
  are fine. It must contain main's `pgweir` schema.
- `branch drop` drops the slot only; the fork is yours to delete.

## Works with

| Platform | Provider | Notes |
|---|---|---|
| Postgres 14–18, self-hosted or Docker | `local` (branches on the same server) or `manual` | `wal_level = logical` needs a restart |
| Neon | `neon` | see [neon](#neon) |
| Amazon RDS / Aurora PostgreSQL | `manual` | set `rds.logical_replication = 1` in the parameter group (reboot) and `GRANT rds_replication TO <role>`; fork with a snapshot restore or an Aurora clone |
| Supabase | `manual` | Supabase Branching builds branches from migrations and seed files, not from production data, so it cannot provide the fork; restore a backup into a separate project or database and use that |
| Other managed Postgres | `manual` | needs logical replication and a role with `REPLICATION`, or the provider's equivalent |
