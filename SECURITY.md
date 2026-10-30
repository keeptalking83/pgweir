# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public
issue for a security problem. You will get an answer within a few days; fixes
are released as patch versions and credited unless you ask otherwise.

## Threat model

pgweir sits between an untrusted writer (an AI agent working on a branch) and
a trusted system of record (main). This section states what pgweir protects,
what it only detects, and what it assumes.

### What pgweir protects

- **Changes reach main only through a merge**, provided the agent cannot
  connect to main on its own (see "Isolating the agent" below). The agent
  works on a fork; `pgweir merge` applies its changes in one transaction,
  under a conflict policy chosen by a human.
- **Merges apply only what was decided.** Every row the plan touches is locked
  with `SELECT ... FOR UPDATE` and compared with the row the plan was computed
  from. If main changed in between, the transaction is rolled back and the
  merge exits with code 3.
- **Captured changes are durable before they are acknowledged.** The branch's
  replication slot is acknowledged only after the change is stored on main, so
  a crash cannot lose or duplicate agent writes.
- **A fork is never main.** `branch create` writes a random marker to main
  after the fork exists and refuses a "fork" that can see it, so a mistyped
  `--branch-dsn` cannot turn main into a branch.

### What pgweir only detects: the ledger is tamper-evident, not tamper-proof

Every applied merge is a leaf of an RFC 6962 Merkle tree stored on main, and
every tree size has a tree head signed with an ed25519 key that is kept
outside the database.

Anyone with write access to main's `pgweir` schema can still delete or rewrite
leaves, tree heads and captured events. pgweir cannot prevent that. What it
provides:

- a record changed in place no longer matches its leaf hash, so `pgweir
  verify` rejects its proof;
- leaves changed together with their hashes no longer match the signed root,
  and a forged root cannot be signed without the private key;
- a rewritten or truncated ledger is not consistent with tree heads published
  earlier, so `pgweir ledger verify-chain` rejects it.

Detection therefore depends on copies the attacker cannot change: proofs and
tree heads handed to third parties, or an export (`pgweir ledger export`) to
an S3 bucket with Object Lock in compliance mode, which you configure
yourself. Without such copies, an operator who controls both main and the
signing key can rewrite history undetected.

### Assumptions

- **Main is a trusted store.** `pgweir.branches.dsn` holds each branch's
  connection string, including its password, in plain text. Anyone who can
  read `pgweir.branches` can connect to every open branch. By default only
  the role that ran `pgweir init` (and superusers) can read it; keep it that
  way, and grant agents only what the tagging function needs (see below).

- **The signing key stays off the database host.** pgweir reads the key from
  a file (`--signing-key` or `PGWEIR_SIGNING_KEY`), refuses files readable by
  group or others, and never stores it. Keep it where your other deployment
  secrets live. The ledger records each public key by key id
  (`pgweir.config`, key `ledger_public_keys`), so tree heads signed by an old
  key stay verifiable after rotation: generate a new key and start using it;
  keep the old public key with your verification material.

- **The role that runs pgweir owns the tracked tables.** It needs
  `REPLICATION`, and for the local provider `CREATEDB`, but never superuser.
  Because it owns the tables, the apply transaction bypasses row-level
  security unless the tables use `FORCE ROW LEVEL SECURITY`.

### Isolating the agent

The connection string printed by `branch create` uses **pgweir's own role**:
the owner of the tracked tables, with `REPLICATION`. With the `local`
provider the fork lives on main's server, and with `neon` the fork keeps the
role's password, so these credentials may also open main. The quickstart
hands them to a simulated agent for brevity; do not do that with an agent you
do not trust.

Give the agent its own role, with privileges **on the fork only**, granted
after `branch create`:

```sql
-- once per server, by a role with CREATEROLE (local provider), or on the
-- Neon branch itself, where a role created with SQL exists only on that branch
CREATE ROLE agent LOGIN PASSWORD '...';

-- on the fork, connected with the connection string from branch create
GRANT USAGE ON SCHEMA public, pgweir TO agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.orders, public.customers TO agent;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO agent;
GRANT EXECUTE ON FUNCTION pgweir.tag(jsonb) TO agent;
```

Then give the agent the fork's connection string with the `agent` role's
credentials. Grants made on the fork do not exist on main. With the `local`
provider the role exists server-wide, so it can still connect to main, but it
has no privileges on main's tables unless you grant them; to keep it out
entirely, `REVOKE CONNECT ON DATABASE <main> FROM PUBLIC` and grant `CONNECT`
only where needed.

A role like this can call `pgweir.tag()` but cannot touch replication slots,
run DDL on tables it does not own, or change replica identity. An agent with
more privileges on the fork can make the branch unmergeable or hide its own
writes, but not write to main:

| Agent privilege on the fork | What it allows | Effect |
|---|---|---|
| `REPLICATION` | dropping or advancing the slot | slot dropped: the branch becomes `FAILED`; slot advanced: skipped writes are never merged |
| table ownership | DDL, `ALTER TABLE ... REPLICA IDENTITY` | capture aborts with `SCHEMA_DRIFT` or `REPLICA_IDENTITY_NOT_FULL` |
| `TRUNCATE` | truncating a tracked table | capture aborts with `TRUNCATE_NOT_SUPPORTED` |

### Run tags are claims, not identities

`pgweir.tag()` records whatever JSON object the agent sends: `run_id`,
`tool_call_id`, `agent` and any other fields. pgweir stores and signs them in
RFC 8785 canonical form (numbers as IEEE 754 doubles). They are useful
attribution from a cooperating agent framework, not authenticated
identities.

### Triggers on main

The apply transaction runs main's triggers, with pgweir's pinned session
settings (`TimeZone=UTC` and others, see `docs/design.md`). If a tracked table
is maintained by a trigger, for example an audit table filled from another
tracked table, the branch's trigger output is merged **and** main's trigger
fires again during apply. Do not track tables that triggers maintain.
