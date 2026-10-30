// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/keeptalking83/pgweir/internal/pgtest"
)

var srv *pgtest.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	s, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration tests need Docker:", err)
		os.Exit(1)
	}
	srv = s
	code := m.Run()
	_ = s.Stop(ctx)
	os.Exit(code)
}

// shopSchema is the fixture most integration tests start from.
const shopSchema = `
CREATE TABLE customers (
  id    bigint PRIMARY KEY,
  email text NOT NULL UNIQUE,
  tier  text NOT NULL DEFAULT 'basic'
);
CREATE TABLE orders (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES customers(id),
  status      text NOT NULL DEFAULT 'NEW',
  amount      numeric(12,2) NOT NULL,
  note        text,
  flag        boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT '2026-01-01 00:00:00+00'
);
INSERT INTO customers SELECT g, 'c' || g || '@example.com', 'basic' FROM generate_series(1, 20) g;
INSERT INTO customers VALUES (99, 'c99@example.com', 'basic'); -- no orders reference it
INSERT INTO orders (customer_id, status, amount)
  SELECT 1 + g % 20, 'PENDING', g * 1.5 FROM generate_series(1, 100) g;
-- A value large enough to be stored out of line (TOAST) and not compressible.
UPDATE orders SET note = (SELECT string_agg(md5(i::text || random()::text), '') FROM generate_series(1, 200) i) WHERE id = 20;
`

type cliResult struct {
	stdout, stderr string
	code           int
}

// pgweir runs the CLI in-process.
func pgweir(t *testing.T, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func mustPgweir(t *testing.T, args ...string) cliResult {
	t.Helper()
	r := pgweir(t, args...)
	if r.code != 0 {
		t.Fatalf("pgweir %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// setup creates a main database with shopSchema, runs init and returns its DSN.
func setup(t *testing.T, extra string) string {
	t.Helper()
	main := srv.NewMain(t, pgtest.UniqueName("main"), shopSchema+extra)
	mustPgweir(t, "init", "--dsn", main, "--tables", "public.customers,public.orders")
	return main
}

func createBranch(t *testing.T, main, name string) string {
	t.Helper()
	r := mustPgweir(t, "branch", "create", name, "--dsn", main, "--provider", "local")
	return strings.TrimSpace(r.stdout)
}

type changesetJSON struct {
	BranchName string `json:"branch_name"`
	BaseLSN    string `json:"base_lsn"`
	EndLSN     string `json:"end_lsn"`
	Rows       []struct {
		Schema string             `json:"schema"`
		Table  string             `json:"table"`
		PK     map[string]*string `json:"pk"`
		Base   map[string]*string `json:"base"`
		Final  map[string]*string `json:"final"`
		Runs   []string           `json:"runs"`
		Ops    int                `json:"ops"`
	} `json:"rows"`
	Runs []map[string]any `json:"runs"`
}

func diffJSON(t *testing.T, main, name string) changesetJSON {
	t.Helper()
	r := mustPgweir(t, "diff", name, "--dsn", main, "--json", "--idle-timeout", "300ms")
	var cs changesetJSON
	if err := json.Unmarshal([]byte(r.stdout), &cs); err != nil {
		t.Fatalf("decode changeset: %v\n%s", err, r.stdout)
	}
	return cs
}

func val(m map[string]*string, col string) string {
	if m == nil {
		return "<row absent>"
	}
	v, ok := m[col]
	if !ok {
		return "<no column>"
	}
	if v == nil {
		return "<null>"
	}
	return *v
}
