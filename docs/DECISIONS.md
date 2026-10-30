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
