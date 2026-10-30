# Non-goals for v0.1

pgweir v0.1 deliberately does not do the following.

- DDL capture or schema merge. Schema drift between branch and main is **detected** (abort with `SCHEMA_DRIFT`), never merged.
- Tables without a primary key.
- Sequence capture or merge (sequence values are never copied from the branch; pgweir only advances main's key sequences past keys it inserted), large objects, partitioned tables (parent or children), unlogged tables, generated columns on the write side.
- A branch that continues after being merged. v0.1 branches are single-merge: after a successful merge the branch is `MERGED` and further diffs are refused; drop it and create a new one.
- Multi-branch merge ordering, rebase, cherry-pick.
- PGlite / edge / offline clients.
- Web UI, MCP server, HTTP API.
- Tiger Data provider (v0.2). v0.1 providers: `local`, `neon`, `manual`.
- Tamper-proof guarantees. Operator with write access to main can alter history; detection requires the exported STH chain or an external anchor.
- Automatic conflict resolution beyond the three policies.
