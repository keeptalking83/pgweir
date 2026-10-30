# Ledger format and verification

Every applied merge becomes one leaf of an append-only Merkle tree
([RFC 6962](https://www.rfc-editor.org/rfc/rfc6962), the construction used by
Certificate Transparency). After each append pgweir signs the new tree head
with an ed25519 key. With the public key, anyone can check offline that a
merge record is part of the ledger and that the ledger only ever grew.

The ledger is **tamper-evident, not tamper-proof**: see
[SECURITY.md](../SECURITY.md) for what that means in practice.

## Canonical JSON

Everything that is hashed or signed is serialized with the JSON
Canonicalization Scheme ([RFC 8785](https://www.rfc-editor.org/rfc/rfc8785)):
object keys sorted, no insignificant whitespace, minimal string escaping. Row
values are always JSON strings (the Postgres text representation) or `null`,
so numbers never need canonicalizing.

## Leaf record

```json
{
  "v": 1,
  "merge_id": "0bfe5bb6-e434-45a3-b4a6-e1b83b258b55",
  "branch_id": "6d1c...", "branch_name": "demo",
  "base_lsn": "0/1E25260", "end_lsn": "0/1E271F8", "main_lsn_after": "0/1E3A0B8",
  "changeset_hash": "<hex sha256 of the canonical changeset>",
  "policy": "SKIP",
  "stats": {"rows_total": 27, "inserted": 5, "updated": 19, "deleted": 2, "skipped": 1, "conflicts": 1},
  "conflicts_hash": "<hex sha256 of the canonical conflicts array>",
  "runs": [{"agent": "inventory-bot", "parent_run_id": null, "run_id": "run_b7265c34", "tool_call_id": "call_1"}],
  "applied_at": "2026-10-30T08:57:44.479Z"
}
```

| Field | Meaning |
|---|---|
| `base_lsn` | the branch's fork point (the slot's consistent point) |
| `end_lsn` | the last branch commit the merge covers |
| `main_lsn_after` | main's WAL position inside the apply transaction |
| `changeset_hash` | SHA-256 of the canonical changeset (`pgweir diff --json`) |
| `conflicts_hash` | SHA-256 of the canonical array of conflicts, in changeset order |
| `runs` | the agent runs that wrote the merged rows, sorted by `run_id` |
| `applied_at` | RFC 3339 UTC with milliseconds; equal to the tree head's timestamp |

The full changeset is not in the leaf. It stays in `pgweir.events` and is
addressed by `changeset_hash`; `pgweir ledger export --include-changesets`
rebuilds it and checks the hash.

```
leaf_hash = SHA-256(0x00 || jcs(record))
```

## Tree and tree heads

Leaves are numbered from 0 without gaps. Interior nodes are
`SHA-256(0x01 || left || right)` (RFC 6962 section 2.1). A signed tree head
(STH) covers the first `tree_size` leaves:

```json
{"v":1,"tree_size":1,"root_hash":"838ced6f...","timestamp":"2026-10-30T08:57:44.479Z","key_id":"f24357979314a688"}
```

```
signature = ed25519.Sign(private_key, jcs(sth))
key_id    = first 16 hex characters of SHA-256(public_key)
```

One STH is written per tree size, inside the apply transaction of the merge
that produced it. Merges of different branches append one at a time under an
advisory lock, so `leaf_index` and `tree_size` stay dense and timestamps
increase with the tree size.

## Inclusion proof

`pgweir prove <merge-id> --out proof.json` writes:

```json
{
  "leaf_index": 0,
  "leaf_record": { ... },
  "leaf_hash": "<hex>",
  "tree_size": 1,
  "audit_path": ["<hex>", "..."],
  "sth": { ... },
  "signature": "<hex>",
  "public_key": "<hex>"
}
```

The proof is against the latest tree head at the time it is produced.

### Verifying

`pgweir verify --proof proof.json --pubkey pgweir.pub` needs no database.
It checks, in order:

1. `leaf_hash == SHA-256(0x00 || jcs(leaf_record))`, so the record is the one
   that was hashed;
2. the audit path leads from `leaf_hash` at `leaf_index` to `sth.root_hash`
   for a tree of `tree_size` leaves (RFC 6962 section 2.1.1);
3. `signature` is a valid ed25519 signature of `jcs(sth)` under the given
   public key, and `sth.key_id` names that key.

The `public_key` inside the proof is ignored unless you pass
`--trust-embedded`. Get the public key from a source the operator of main
does not control. Exit code 0 means valid, 1 invalid, with a one-line reason.

The checks use only SHA-256, ed25519 and RFC 8785, so they can be
reimplemented in any language.

## Append-only checks

`pgweir ledger consistency --from 3 --to 7` prints an RFC 6962 consistency
proof between two signed tree heads.

`pgweir ledger verify-chain --sths <file-or-dir> --pubkey pgweir.pub` takes
tree heads you saved earlier (a JSON array of signed tree heads, or a
directory such as an export), verifies every signature, and proves that each
one is a prefix of the ledger now on main. A ledger that was rewritten or
truncated after you saved a tree head fails this check.

Saved tree heads are what make tampering detectable. Publish them where the
operator of main cannot change them: an Object Lock bucket, a transparency
log, a git repository, a ticket.

## Export layout

`pgweir ledger export --dir <path>` or `--s3 s3://bucket/prefix`:

```
leaves/00000000-00000999.json      [{"leaf_index":0,"leaf_hash":"...","record":{...}}, ...]
leaves/00001000-00001036.json
sth/1.json ... sth/1037.json       {"sth":{...},"signature":"...","public_key":"..."}
changesets/<merge_id>.json         canonical changeset (with --include-changesets)
```

- A leaf file holds at most `--segment` leaves (default 1000) and ends on a
  segment boundary or at the last leaf. Later exports start at the first leaf
  not yet exported, so no file is ever rewritten.
- Objects are canonical JSON and are never overwritten. On S3 pgweir reads
  before writing and writes with `If-None-Match: *`. An existing object with
  different content is an error: the ledger changed after it was exported.
- pgweir does not configure buckets. For tamper evidence, enable S3 Object
  Lock in compliance mode on the bucket yourself.
- S3 credentials and region come from the standard AWS configuration.
  `AWS_ENDPOINT_URL_S3` selects an S3-compatible endpoint (path-style).

## Storage

| Table | Contents |
|---|---|
| `pgweir.ledger_leaves` | `leaf_index`, `leaf_hash`, `record` |
| `pgweir.ledger_sth` | `tree_size`, `root_hash`, `timestamp`, `key_id`, `signature` |
| `pgweir.config` `ledger_compact_range` | the RFC 6962 compact range of the current tree, so an append costs O(log n) |
| `pgweir.config` `ledger_public_keys` | public key by key id |

When the stored compact range is missing or does not reproduce the latest
signed root, pgweir rebuilds it from the leaf hashes. If the rebuilt root does
not match the signed root either, the leaves were altered and pgweir refuses
to append to the ledger.
