# Decisions

Append-only log of decisions made where the specification is silent or had to
be corrected after verification. Each entry: what, why, alternatives rejected.

---

## 2026-10-30 — Go toolchain floor is 1.25

**What.** `go.mod` declares `go 1.25.0`.
**Why.** `github.com/jackc/pgx/v5` v5.11 and `testcontainers-go` v0.44 both
require Go 1.25. The specification asks for "Go 1.23+", which 1.25 satisfies.
**Rejected.** Pinning older dependency versions to keep a 1.23 floor: older pgx
releases miss fixes, and the floor has no user-visible benefit for a static
binary.

## 2026-10-30 — `internal/errs` package for coded errors

**What.** A small package outside the layout of the specification holds the
error codes (`SCHEMA_DRIFT`, `TRUNCATE_NOT_SUPPORTED`, ...) and maps them to the
process exit codes of §10.
**Why.** Codes are raised in `capture`, `apply`, `ledger` and `cli`; putting the
type in any one of them would create import cycles or odd dependencies.
**Rejected.** Sentinel errors per package (exit code mapping would be spread over
the CLI); putting the type in `internal/meta` (meta is build information).

## 2026-10-30 — `pgweir.tag()` casts through text

**What.** The function body is `SELECT pg_logical_emit_message(true, 'pgweir', t::text)::text::void`.
**Why.** `pg_logical_emit_message` returns `pg_lsn`, and there is no cast from
`pg_lsn` to `void`; I/O conversion casts only exist to and from string types.
**Rejected.** Returning the LSN to the agent (changes the documented signature).

## 2026-10-30 — Primary-key columns come from main, not from Relation key flags

**What.** Capture takes the primary-key columns of each table from main's
catalog (the `tracked_tables` config and the per-capture introspection), and
checks the Relation message's replica identity byte is `f`.
**Why.** With `REPLICA IDENTITY FULL`, pgoutput flags *every* column as part of
the key (`logicalrep_write_attrs`), so the key flag cannot identify the primary
key. The branch's primary key is compared with main's at `branch create`.
**Rejected.** Using the key flags (spec §7 step 6 wording): it would report
schema drift on every table.

## 2026-10-30 — Capture termination and slot progress

**What.** While idle, capture sends a standby status update with
`ReplyRequested` every 500 ms. The server answers with a keepalive whose WAL
position is how far decoding has read. Capture stops when a commit at or past
the target LSN has been persisted, or when a keepalive position at or past the
target has been seen and no data arrived for `--idle-timeout`. Before
returning, the last keepalive position seen outside a transaction is stored as
`flushed_lsn` and acknowledged.
**Why.** Every transaction committed before that position has already been
delivered and persisted, so acknowledging it is safe; without it, a branch on a
busy shared server (local provider) would pin WAL until the agent's next
commit. Idempotency is unchanged: transactions with commit LSN at or below
`flushed_lsn` are skipped.
**Rejected.** Relying on the idle timeout alone (cannot tell "caught up" from
"slow"); acknowledging only commit LSNs (WAL retention).

## 2026-10-30 — Event persistence

**What.** Events of one branch transaction are inserted with
`INSERT ... SELECT ... FROM jsonb_array_elements($4)` in chunks of 2000, then
`capture_progress` is updated, all in one main transaction.
**Why.** One round trip per chunk, no COPY type mapping for `pg_lsn`/`jsonb`.

## 2026-10-30 — Branch names

**What.** Branch names match `^[a-z][a-z0-9_]{0,47}$`.
**Why.** The name becomes the slot name `pw_<name>` (slot names allow only
lowercase letters, digits and underscores, at most 63 bytes) and the local
provider's database name.

## 2026-10-30 — Invalid tags abort capture

**What.** A `pgweir` message that is not a JSON object with a non-empty string
`run_id` aborts capture with `INVALID_TAG`.
**Why.** The spec requires `run_id`; silently attributing writes to the wrong
run would corrupt the audit trail. The branch then has to be dropped, like a
branch with a TRUNCATE.

## 2026-10-30 — Run metadata in the changeset

**What.** The changeset's `runs` has one object per `run_id` used by its rows:
the first tag seen for that `run_id` (by seq), canonicalized, with
`parent_run_id`, `tool_call_id` and `agent` present (null when absent). Extra
fields the agent sent are kept.

## 2026-10-30 — Changeset `tables` lists every tracked table

**What.** `tables` contains all tracked tables (main's definitions), not only
the ones with changed rows. Because the definitions are part of the hashed
changeset and main's schema may change after a merge, `pgweir.merges` stores
them in an extra column `changeset_tables` so `ledger export
--include-changesets` can rebuild the exact changeset later.

## 2026-10-30 — Re-running init

**What.** `init` is idempotent. Changing the set of tracked tables is refused
while any branch is OPEN.
**Why.** Open branches were set up (publication, replica identity) for the old
set.

## 2026-10-30 — Missing slot marks the branch FAILED

**What.** If `START_REPLICATION` reports that the slot does not exist, the
branch is set to `FAILED` and capture fails with `SLOT_MISSING`.
**Why.** Changes since the last capture cannot be recovered (for example after
Neon removed an inactive slot).

## 2026-10-30 — Local provider drop

**What.** `DROP DATABASE`, retried `WITH (FORCE)` when sessions are still
connected. The provider refuses to drop a database whose name lacks the `pw_`
prefix.

## 2026-10-30 — `diff` output

**What.** Up to 50 rows per table; `->` instead of `→` in the human listing;
`diff` refuses branches that are not OPEN.
