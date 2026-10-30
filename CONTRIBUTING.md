# Contributing to pgweir

Thank you for helping. pgweir keeps a deliberately small scope, so please read
[NON-GOALS.md](NON-GOALS.md) before proposing a feature, and open an issue to
discuss anything larger than a fix.

## Development setup

You need Go 1.25 or later and Docker (the integration tests start Postgres
containers).

```sh
git clone https://github.com/keeptalking83/pgweir
cd pgweir
go build ./...
go test ./...                       # unit tests, no database needed
go test -tags integration ./...     # integration tests, needs Docker
```

The integration tests use `postgres:16` by default. Run them against another
version with:

```sh
PGWEIR_TEST_PG_IMAGE=postgres:14 go test -tags integration ./...
```

CI runs Postgres 14 to 18.

Lint with [golangci-lint](https://golangci-lint.run) v2:

```sh
golangci-lint run ./...
golangci-lint run --build-tags integration ./...
```

To run the end-to-end demo the way CI does:

```sh
go build -o /usr/local/bin/pgweir ./cmd/pgweir     # or anywhere on PATH
docker compose -f examples/docker-compose.yml up -d --wait
pip install "psycopg[binary]"
./examples/conflict_demo.sh
docker compose -f examples/docker-compose.yml down -v
```

## Layout

| Path | Contents |
|---|---|
| `cmd/pgweir` | entry point |
| `internal/cli` | cobra commands; end-to-end integration tests |
| `internal/capture` | logical replication consumer and pgoutput decoding |
| `internal/changeset` | collapsed row model, canonical JSON, hashing |
| `internal/merge` | three-way merge engine (pure, no I/O) |
| `internal/apply` | reading main, plan computation, the apply transaction |
| `internal/ledger` | Merkle ledger, signing, proofs, export |
| `internal/branch`, `internal/provider` | branch lifecycle and fork providers |
| `internal/schema`, `internal/pg` | catalog introspection, connections |
| `sql/001_schema.sql` | the metadata schema installed on main |
| `docs/` | design, providers, agent protocol, ledger format, decision log |

## Rules of the code base

- `internal/merge` and the core of `internal/ledger` are pure. Change them
  test-first; every row of the decision table in `docs/design.md` has a test.
- Anything that talks to Postgres gets an integration test
  (`//go:build integration`).
- Never weaken a test to make it pass. If behaviour must change, change the
  test in the same commit and explain why.
- When you make a choice the documentation does not cover, append a dated
  entry to [docs/DECISIONS.md](docs/DECISIONS.md): what, why, and the
  alternatives you rejected.
- Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`.
- User-facing strings are in English. No emojis in CLI output.
- Machine output (JSON, connection strings, merge ids, proofs) goes to stdout;
  human output goes to stderr.
- Never describe the ledger as tamper-proof. It is tamper-evident.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org):
`feat(capture): ...`, `fix(apply): ...`, `docs: ...`. Keep commits small and
pull requests focused on one change. CI must be green: vet, lint, unit tests
with `-race`, the integration matrix and the demo.

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
