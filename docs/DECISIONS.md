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

## 2026-10-30 — `end_lsn` is the last captured commit (supersedes part of "Capture termination and slot progress")

**What.** `capture_progress.flushed_lsn`, and therefore a merge's `end_lsn`,
is the commit LSN of the last persisted branch transaction (the slot's
consistent point before the first one). The slot is still acknowledged up to
the last keepalive position seen outside a transaction, but that position is
not persisted.
**Why.** Persisting the keepalive position made `end_lsn`, the changeset hash
and `merge --out plan.json` change on every run even when the branch was
idle, breaking the byte-identical plan requirement. Acknowledging ahead of
`flushed_lsn` is safe: every transaction committed before that position was
persisted, and the server never resends anything below its confirmed position.

## 2026-10-30 — Main values are read in the text result format

**What.** Main's rows are fetched with plain column references and the text
result format of the extended protocol (`pgx.QueryResultFormats{TextFormatCode}`),
not with `::text` casts (spec §1.7, §9.1).
**Why.** The text result format is the type's output function, exactly what
pgoutput sends. A `::text` cast is not always the output function:
`true::text` is `'true'` while `boolout` gives `'t'`, so every boolean column
would conflict.

## 2026-10-30 — Parameters are bound as text and cast twice

**What.** Values are bound as `$n::text::<type>`; keys are matched with
`(pk...) IN (SELECT * FROM unnest($1::text[]::<type>[], ...))` (the same form
for single and composite keys).
**Why.** Binding as `text` lets Postgres apply the type's input function to
the exact text representation, for every type including enums, domains and
arrays, without pgx having to encode Go strings into each binary format.

## 2026-10-30 — Merge engine details

- A row whose resolved target equals main's current row is a no-op, not an
  UPDATE (for example "branch and main made the same change"). UPDATE
  statements set only the columns that differ from main's row.
- `stats.skipped` counts rows without an action: no-ops, and conflicts under
  `SKIP` or `FAIL`. `rows_total = inserted + updated + deleted + skipped`.
- Under `FAIL` the plan still lists the actions of non-conflicting rows (for
  display); nothing is applied.
- The canonical plan (`--out`) contains the branch name, changeset hash,
  policy, status, stats, actions, conflicts and warnings; no merge id or
  timestamps, so it is byte-identical across runs.
- The warning for `ACCEPT` on `INSERT_INSERT` with a sequence-generated key is
  printed in the plan summary and stored in the plan's `warnings`.

## 2026-10-30 — Merge command behaviour

- The merge id is printed on stdout (like the DSN of `branch create`), so
  scripts can capture it; the summary goes to stderr. `--json` prints a JSON
  object instead.
- Conflicts under `FAIL` exit 5 even with `--dry-run`.
- Declining the confirmation prompt exits 2, like a missing `--yes` on a
  non-TTY.
- An UPDATE that does not affect exactly one row aborts with `RETRY`.
- A merge with no actions is still applied and recorded in the ledger: the
  record documents that the branch changed nothing on main.

## 2026-10-30 — Ledger details

- `applied_at` and the tree head timestamp are the same instant, taken after
  the advisory lock and forced to increase by at least 1 ms over the previous
  tree head.
- Each signing key's public key is stored in `pgweir.config` under
  `ledger_public_keys` (by key id), so `prove` can embed it.
- Records are stored as `jsonb` and re-canonicalized (RFC 8785) when read.
- If the stored compact range is missing, of the wrong size or does not
  reproduce the latest signed root, it is rebuilt from the leaf hashes. If the
  rebuilt root does not match either, the ledger was altered and the merge is
  refused rather than signing a tree over altered leaves.
- `ledger verify-chain` reads the current ledger from main (`--dsn`) and
  checks that every given signed tree head (a JSON array, or a directory such
  as an export) verifies with `--pubkey` and is a prefix of the current tree
  (a consistency proof from each head to the current head). It needs no
  consistency proofs in the input.
- `ledger consistency --to` defaults to the latest tree size.
- `prove` and `ledger` commands print indented JSON.
- `keygen --out` defaults to `pgweir.key` and never overwrites. Signing keys
  readable by group or others are refused.

## 2026-10-30 — Export layout

Leaf files cover at most `--segment` leaves and end on segment boundaries;
the first file of a later export starts at the first leaf not yet exported, so
a file is never rewritten (for example `00000000-00000036.json`, then
`00000037-00000999.json`). Exported objects are canonical JSON. An existing
object with different content is an error. With `--include-changesets`, each
applied merge's changeset is rebuilt from `pgweir.events` (events up to
`end_lsn`) and `changeset_tables`, and must reproduce `changeset_hash`.

## 2026-10-30 — Neon provider

**What.** Neon API v2 as verified on 2026-10-30 (paths, payloads and sources
in `docs/providers.md`). The fork is a Neon branch named `pw_<name>` with one
`read_write` endpoint; pgweir waits for the create operations to finish, asks
for a direct (`pooled=false`) connection URI for main's role and database, and
retries connecting until the compute accepts connections. Requests answered
with 423, 429 or 503 are retried with exponential backoff. When main's host is
a Neon endpoint (`ep-...`), the parent branch must be that endpoint's branch,
otherwise `branch create` fails and names the right `--parent`.
`PGWEIR_NEON_API_URL` overrides the API base URL (tests, proxies).
**Why.** Forking the default branch while main lives on another branch would
silently merge against the wrong base. Replication connections cannot go
through Neon's pooler.
**Rejected.** Disabling scale-to-zero on the fork (`suspend_timeout_seconds`):
not allowed on every plan; Neon keeps inactive slots for about 40 hours, which
is documented as the capture deadline instead.

## 2026-10-30 — S3 export and its tests

**What.** The S3 sink reads an object before writing it and writes with
`If-None-Match: *`; an existing object is compared byte for byte. When
`AWS_ENDPOINT_URL_S3` or `AWS_ENDPOINT_URL` is set, requests are path-style.
The sink is tested against an in-process fake S3 server, not MinIO.
**Why.** The read-before-write protects stores that ignore conditional
writes. MinIO no longer publishes container images (Docker Hub and quay.io
pulls fail as of 2026-10-30), and the specification forbids tests that need
the network for anything but the Postgres image.

## 2026-10-30 — Postgres 18 in the CI matrix

**What.** The integration matrix runs Postgres 14, 15, 16, 17 and 18.
**Why.** Postgres 18 is the current release and the whole suite passes on it.
The specification's minimum (14) is unchanged.
