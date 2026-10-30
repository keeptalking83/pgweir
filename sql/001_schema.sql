-- SPDX-License-Identifier: Apache-2.0
--
-- pgweir metadata schema, installed on main by `pgweir init`. Idempotent.
-- Forks inherit this schema; on a branch only pgweir.tag() is used.

CREATE SCHEMA IF NOT EXISTS pgweir;

CREATE TABLE IF NOT EXISTS pgweir.config (
  key   text PRIMARY KEY,
  value jsonb NOT NULL
);
-- keys: 'tracked_tables' (jsonb array of {"schema":..,"name":..,"pk":[..]}),
--       'publication' (text), 'schema_version' (int),
--       'ledger_compact_range' ({"size":n,"hashes":["hex",..]})

CREATE TABLE IF NOT EXISTS pgweir.branches (
  id           uuid PRIMARY KEY,
  name         text UNIQUE NOT NULL,
  provider     text NOT NULL,                     -- local | neon | manual
  provider_ref jsonb NOT NULL DEFAULT '{}'::jsonb, -- provider-specific ids
  dsn          text NOT NULL,                     -- branch connection string (main is a trusted store, see SECURITY.md)
  slot_name    text NOT NULL,
  base_lsn     pg_lsn NOT NULL,                   -- consistent point of the slot = fork base
  status       text NOT NULL CHECK (status IN ('OPEN','MERGED','DROPPED','FAILED')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Raw captured events, append-only, one row per row-operation. Persisted before the slot is acknowledged.
CREATE TABLE IF NOT EXISTS pgweir.events (
  branch_id    uuid NOT NULL REFERENCES pgweir.branches(id),
  seq          bigint NOT NULL,                   -- monotonic per branch
  xid          bigint NOT NULL,
  commit_lsn   pg_lsn NOT NULL,
  op           text NOT NULL CHECK (op IN ('I','U','D')),
  table_schema text NOT NULL,
  table_name   text NOT NULL,
  pk           jsonb NOT NULL,                    -- {"col": "text value", ...}
  old_row      jsonb,                             -- full row as text values, null for I
  new_row      jsonb,                             -- full row as text values, null for D
  run          jsonb NOT NULL,                    -- tag in effect, or {"run_id":"untagged"}
  PRIMARY KEY (branch_id, seq)
);

CREATE TABLE IF NOT EXISTS pgweir.capture_progress (
  branch_id    uuid PRIMARY KEY REFERENCES pgweir.branches(id),
  last_seq     bigint NOT NULL DEFAULT 0,
  flushed_lsn  pg_lsn NOT NULL                    -- everything up to here is persisted; acknowledged to the slot
);

CREATE TABLE IF NOT EXISTS pgweir.merges (
  id              uuid PRIMARY KEY,
  branch_id       uuid NOT NULL REFERENCES pgweir.branches(id),
  policy          text NOT NULL CHECK (policy IN ('FAIL','SKIP','ACCEPT')),
  dry_run         boolean NOT NULL,
  status          text NOT NULL CHECK (status IN ('COMPUTED','APPLIED','ABORTED','CONFLICT')),
  end_lsn         pg_lsn NOT NULL,                -- branch LSN covered by this merge
  main_lsn_after  pg_lsn,                         -- pg_current_wal_lsn() on main inside the apply transaction
  changeset_hash  bytea NOT NULL,                 -- sha256 of the canonical changeset
  changeset_tables jsonb NOT NULL DEFAULT '[]'::jsonb, -- "tables" of the canonical changeset, to rebuild it later
  stats           jsonb NOT NULL,
  leaf_index      bigint,                         -- set when APPLIED and recorded in the ledger
  started_at      timestamptz NOT NULL DEFAULT now(),
  finished_at     timestamptz
);

CREATE TABLE IF NOT EXISTS pgweir.conflicts (
  merge_id     uuid NOT NULL REFERENCES pgweir.merges(id),
  table_schema text NOT NULL,
  table_name   text NOT NULL,
  pk           jsonb NOT NULL,
  kind         text NOT NULL CHECK (kind IN ('INSERT_INSERT','DELETE_MODIFY','MODIFY_DELETE','CELL_CONFLICT')),
  cells        jsonb,                             -- for CELL_CONFLICT: [{"column":..,"base":..,"ours":..,"theirs":..}]
  base_row     jsonb,
  ours_row     jsonb,
  theirs_row   jsonb,
  resolution   text NOT NULL CHECK (resolution IN ('UNRESOLVED','SKIPPED','ACCEPTED_THEIRS')),
  PRIMARY KEY (merge_id, table_schema, table_name, pk)
);

CREATE TABLE IF NOT EXISTS pgweir.ledger_leaves (
  leaf_index  bigint PRIMARY KEY,                 -- 0-based, dense
  leaf_hash   bytea NOT NULL,                     -- RFC 6962 leaf hash
  record      jsonb NOT NULL,                     -- canonical merge record
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS pgweir.ledger_sth (
  tree_size   bigint PRIMARY KEY,
  root_hash   bytea NOT NULL,
  timestamp   timestamptz NOT NULL,
  key_id      text NOT NULL,                      -- first 16 hex chars of sha256(public key)
  signature   bytea NOT NULL                      -- ed25519 over the canonical STH JSON
);

-- Agent tagging helper. transactional=true so the message is ordered inside the agent's transaction.
CREATE OR REPLACE FUNCTION pgweir.tag(t jsonb) RETURNS void
LANGUAGE sql VOLATILE AS $$
  SELECT pg_catalog.pg_logical_emit_message(true, 'pgweir', t::text)::text::void;
$$;
