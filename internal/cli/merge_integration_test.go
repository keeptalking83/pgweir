// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/pgtest"
	schemasql "github.com/keeptalking83/pgweir/sql"
)

const idle = "300ms"

func keygen(t *testing.T) (keyPath, pub string) {
	t.Helper()
	keyPath = filepath.Join(t.TempDir(), "pgweir.key")
	r := mustPgweir(t, "keygen", "--out", keyPath)
	return keyPath, strings.TrimSpace(r.stdout)
}

func mergeArgs(main, name, key, policy string, extra ...string) []string {
	args := []string{"merge", name, "--dsn", main, "--policy", policy, "--idle-timeout", idle}
	if key != "" {
		args = append(args, "--signing-key", key)
	}
	return append(args, extra...)
}

// agentWork is the work of examples/agent_demo.py: three tagged transactions.
func agentWork(t *testing.T, dsn string) {
	t.Helper()
	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"run_demo","tool_call_id":"call_1","agent":"inventory-bot"}');
UPDATE orders SET status = 'SHIPPED' WHERE id BETWEEN 1 AND 20;
COMMIT;`)
	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"run_demo","tool_call_id":"call_2","agent":"inventory-bot"}');
INSERT INTO orders (customer_id, status, amount, note)
  SELECT g, 'NEW', 10 * g, 'created by agent' FROM generate_series(1, 5) g;
COMMIT;`)
	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"run_demo","tool_call_id":"call_3","agent":"inventory-bot"}');
DELETE FROM orders WHERE id IN (90, 91);
COMMIT;`)
}

func TestMergeEndToEnd(t *testing.T) {
	key, pub := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "demo")
	// Different database-level time zones on main and branch must not create
	// spurious conflicts (deletes compare the whole row).
	pgtest.Exec(t, main, "ALTER DATABASE "+pgtest.Query(t, main, "SELECT current_database()")+" SET timezone = 'America/New_York'")
	pgtest.Exec(t, dsn, "ALTER DATABASE "+pgtest.Query(t, dsn, "SELECT current_database()")+" SET timezone = 'Asia/Tokyo'")

	agentWork(t, dsn)
	pgtest.Exec(t, main, "UPDATE orders SET note = 'touched on main' WHERE id = 5") // disjoint cell
	pgtest.Exec(t, main, "UPDATE orders SET status = 'CANCELLED' WHERE id = 7")     // same cell

	// Without a key, only --dry-run works.
	if r := pgweir(t, mergeArgs(main, "demo", "", "skip", "--yes")...); r.code == 0 || !strings.Contains(r.stderr, "signing key") {
		t.Fatalf("merge without key: exit %d\n%s", r.code, r.stderr)
	}

	r := pgweir(t, mergeArgs(main, "demo", key, "fail", "--yes")...)
	if r.code != errs.ExitConflicts {
		t.Fatalf("fail policy: exit %d, want 5\n%s", r.code, r.stderr)
	}
	failID := strings.TrimSpace(r.stdout)
	c := mustPgweir(t, "conflicts", failID, "--dsn", main)
	if lines := strings.Split(strings.TrimSpace(c.stdout), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `public.orders id="7" CELL_CONFLICT`) {
		t.Fatalf("conflicts:\n%s", c.stdout)
	}

	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "plan1.json"), filepath.Join(dir, "plan2.json")
	mustPgweir(t, mergeArgs(main, "demo", "", "skip", "--dry-run", "--out", p1)...)
	mustPgweir(t, mergeArgs(main, "demo", "", "skip", "--dry-run", "--out", p2)...)
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if !bytes.Equal(b1, b2) || len(b1) == 0 {
		t.Fatal("plan.json is not byte-identical across runs")
	}

	if r := pgweir(t, mergeArgs(main, "demo", key, "skip")...); r.code != errs.ExitNeedsConfirm {
		t.Fatalf("non-TTY without --yes: exit %d, want 2\n%s", r.code, r.stderr)
	}

	r = mustPgweir(t, mergeArgs(main, "demo", key, "skip", "--yes")...)
	mergeID := strings.TrimSpace(r.stdout)
	if !strings.Contains(r.stderr, "applied merge") {
		t.Fatalf("merge output:\n%s", r.stderr)
	}
	check := func(sql, want string) {
		t.Helper()
		if got := pgtest.Query(t, main, sql); got != want {
			t.Errorf("%s = %q, want %q", sql, got, want)
		}
	}
	check("SELECT status || '/' || note FROM orders WHERE id = 5", "SHIPPED/touched on main")
	check("SELECT status FROM orders WHERE id = 7", "CANCELLED")
	check("SELECT count(*)::text FROM orders WHERE status = 'SHIPPED'", "19")
	check("SELECT count(*)::text FROM orders WHERE note = 'created by agent'", "5")
	check("SELECT count(*)::text FROM orders WHERE id IN (90, 91)", "0")
	check("SELECT status FROM pgweir.branches WHERE name = 'demo'", "MERGED")
	check("SELECT status FROM pgweir.merges WHERE id = '"+mergeID+"'", "APPLIED")
	// main's identity sequence was advanced past the agent's inserts
	pgtest.Exec(t, main, "INSERT INTO orders (customer_id, status, amount) VALUES (1, 'NEW', 10)")
	check("SELECT max(id)::text FROM orders", "106")

	// Proof round trip.
	proof := filepath.Join(dir, "proof.json")
	mustPgweir(t, "prove", mergeID, "--dsn", main, "--out", proof)
	v := mustPgweir(t, "verify", "--proof", proof, "--pubkey", pub)
	if !strings.HasPrefix(v.stdout, "valid: merge "+mergeID+" is leaf 0") {
		t.Fatalf("verify: %s", v.stdout)
	}
	other, otherPub := keygen(t)
	_ = other
	if r := pgweir(t, "verify", "--proof", proof, "--pubkey", otherPub); r.code != 1 || !strings.HasPrefix(r.stdout, "invalid:") {
		t.Fatalf("verify with another key: exit %d %s", r.code, r.stdout)
	}

	// The branch is single-merge.
	if r := pgweir(t, "diff", "demo", "--dsn", main); r.code == 0 {
		t.Fatal("diff after merge must be refused")
	}
}

func TestMergeRetryWhenMainChanges(t *testing.T) {
	key, _ := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "retry")
	pgtest.Exec(t, dsn, "UPDATE orders SET status = 'SHIPPED' WHERE id IN (1, 2)")

	testHooks.beforeApply = func(ctx context.Context) error {
		pgtest.Exec(t, main, "UPDATE orders SET note = 'raced' WHERE id = 1")
		return nil
	}
	r := pgweir(t, mergeArgs(main, "retry", key, "skip", "--yes")...)
	testHooks.beforeApply = nil
	if r.code != errs.ExitRetry || !strings.Contains(r.stderr, "main changed") {
		t.Fatalf("exit %d, want 3\n%s", r.code, r.stderr)
	}
	if got := pgtest.Query(t, main, "SELECT count(*)::text FROM pgweir.merges WHERE status = 'ABORTED'"); got != "1" {
		t.Fatalf("aborted merges: %s", got)
	}
	// Rerun: the row merges cleanly (disjoint cells).
	mustPgweir(t, mergeArgs(main, "retry", key, "skip", "--yes")...)
	if got := pgtest.Query(t, main, "SELECT status || '/' || note FROM orders WHERE id = 1"); got != "SHIPPED/raced" {
		t.Fatalf("row 1: %s", got)
	}
}

func TestMergePrimaryKeyRace(t *testing.T) {
	key, _ := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "pkrace")
	pgtest.Exec(t, dsn, "INSERT INTO orders (customer_id, status, amount) VALUES (1, 'NEW', 1)") // id 101
	testHooks.afterVerify = func(ctx context.Context) error {
		pgtest.Exec(t, main, "INSERT INTO orders (id, customer_id, status, amount) OVERRIDING SYSTEM VALUE VALUES (101, 2, 'MAIN', 2)")
		return nil
	}
	r := pgweir(t, mergeArgs(main, "pkrace", key, "skip", "--yes")...)
	testHooks.afterVerify = nil
	if r.code != errs.ExitRetry {
		t.Fatalf("exit %d, want 3\n%s", r.code, r.stderr)
	}
	// Rerun: now an INSERT_INSERT conflict, skipped.
	r = mustPgweir(t, mergeArgs(main, "pkrace", key, "skip", "--yes")...)
	if !strings.Contains(r.stderr, "INSERT_INSERT") {
		t.Fatalf("expected INSERT_INSERT:\n%s", r.stderr)
	}
}

func TestMergeForeignKeyOrder(t *testing.T) {
	key, _ := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "fkorder")
	// New parent, children re-pointed to it, old parent deleted.
	pgtest.Exec(t, dsn, `BEGIN;
INSERT INTO customers VALUES (1000, 'new@example.com', 'gold');
UPDATE orders SET customer_id = 1000 WHERE customer_id = 3;
DELETE FROM customers WHERE id = 3;
INSERT INTO orders (customer_id, status, amount) VALUES (1000, 'NEW', 5);
COMMIT;`)
	mustPgweir(t, mergeArgs(main, "fkorder", key, "fail", "--yes")...)
	if got := pgtest.Query(t, main, "SELECT count(*)::text FROM orders WHERE customer_id = 1000"); got != "6" {
		t.Fatalf("orders of new customer: %s", got)
	}
	if got := pgtest.Query(t, main, "SELECT count(*)::text FROM customers WHERE id = 3"); got != "0" {
		t.Fatal("old parent not deleted")
	}
}

func TestMergeForeignKeyViolationAborts(t *testing.T) {
	key, _ := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "fkviol")
	pgtest.Exec(t, dsn, "INSERT INTO orders (customer_id, status, amount) VALUES (99, 'NEW', 1)")
	pgtest.Exec(t, main, "DELETE FROM customers WHERE id = 99")
	r := pgweir(t, mergeArgs(main, "fkviol", key, "skip", "--yes")...)
	if r.code != errs.ExitPGAborted || !strings.Contains(r.stderr, "foreign key") {
		t.Fatalf("exit %d, want 4\n%s", r.code, r.stderr)
	}
	if got := pgtest.Query(t, main, "SELECT status FROM pgweir.branches WHERE name = 'fkviol'"); got != "OPEN" {
		t.Fatalf("branch status %s", got)
	}
}

func TestMergeAcceptPolicy(t *testing.T) {
	key, _ := keygen(t)
	main := setup(t, "")
	dsn := createBranch(t, main, "accept")
	pgtest.Exec(t, dsn, "UPDATE orders SET status = 'BRANCH' WHERE id = 1; DELETE FROM orders WHERE id = 2; UPDATE orders SET status = 'B3' WHERE id = 3;")
	pgtest.Exec(t, main, "UPDATE orders SET status = 'MAIN' WHERE id = 1; UPDATE orders SET note = 'kept' WHERE id = 2; DELETE FROM orders WHERE id = 3;")
	r := mustPgweir(t, mergeArgs(main, "accept", key, "accept", "--yes")...)
	if !strings.Contains(r.stderr, "3 conflicts") {
		t.Fatalf("output:\n%s", r.stderr)
	}
	for sql, want := range map[string]string{
		"SELECT status FROM orders WHERE id = 1":                                           "BRANCH",
		"SELECT count(*)::text FROM orders WHERE id = 2":                                   "0",
		"SELECT status FROM orders WHERE id = 3":                                           "B3",
		"SELECT count(*)::text FROM pgweir.conflicts WHERE resolution = 'ACCEPTED_THEIRS'": "3",
	} {
		if got := pgtest.Query(t, main, sql); got != want {
			t.Errorf("%s = %q, want %q", sql, got, want)
		}
	}
}

// TestConcurrentMergesAndLedger merges two branches at the same time, then
// checks dense leaf indices, export, verify-chain and tamper detection.
func TestConcurrentMergesAndLedger(t *testing.T) {
	key, pub := keygen(t)
	main := setup(t, "")
	a := createBranch(t, main, "left")
	b := createBranch(t, main, "right")
	pgtest.Exec(t, a, "UPDATE orders SET status = 'L' WHERE id = 1")
	pgtest.Exec(t, b, "UPDATE orders SET status = 'R' WHERE id = 2")

	var wg sync.WaitGroup
	results := make([]cliResult, 2)
	for i, name := range []string{"left", "right"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = pgweir(t, mergeArgs(main, name, key, "fail", "--yes")...)
		}(i, name)
	}
	wg.Wait()
	for _, r := range results {
		if r.code != 0 {
			t.Fatalf("concurrent merge failed: %s", r.stderr)
		}
	}
	if got := pgtest.Query(t, main, "SELECT string_agg(leaf_index::text, ',' ORDER BY leaf_index) FROM pgweir.ledger_leaves"); got != "0,1" {
		t.Fatalf("leaf indices %s", got)
	}

	exp := t.TempDir()
	mustPgweir(t, "ledger", "export", "--dsn", main, "--dir", exp, "--include-changesets")
	for _, f := range []string{"leaves/00000000-00000001.json", "sth/1.json", "sth/2.json"} {
		if _, err := os.Stat(filepath.Join(exp, f)); err != nil {
			t.Fatalf("export missing %s", f)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(exp, "changesets")); len(entries) != 2 {
		t.Fatalf("changesets exported: %d", len(entries))
	}
	r := mustPgweir(t, "ledger", "export", "--dsn", main, "--dir", exp, "--include-changesets")
	if !strings.Contains(r.stderr, "0 leaves in 0 new leaf files, 0 new tree heads, 0 new changesets") {
		t.Fatalf("second export wrote objects:\n%s", r.stderr)
	}
	mustPgweir(t, "ledger", "verify-chain", "--dsn", main, "--sths", exp, "--pubkey", pub)
	sth := mustPgweir(t, "ledger", "sth", "--dsn", main)
	var head struct {
		STH struct {
			TreeSize int `json:"tree_size"`
		} `json:"sth"`
	}
	if err := json.Unmarshal([]byte(sth.stdout), &head); err != nil || head.STH.TreeSize != 2 {
		t.Fatalf("ledger sth: %s", sth.stdout)
	}
	cons := mustPgweir(t, "ledger", "consistency", "--dsn", main, "--from", "1", "--to", "2")
	if !strings.Contains(cons.stdout, `"proof"`) {
		t.Fatalf("consistency: %s", cons.stdout)
	}

	// Tamper with a record: proofs of that merge fail, export detects it.
	mergeID := pgtest.Query(t, main, "SELECT id::text FROM pgweir.merges WHERE leaf_index = 0")
	pgtest.Exec(t, main, `UPDATE pgweir.ledger_leaves SET record = jsonb_set(record, '{policy}', '"ACCEPT"') WHERE leaf_index = 0`)
	proof := filepath.Join(t.TempDir(), "proof.json")
	mustPgweir(t, "prove", mergeID, "--dsn", main, "--out", proof)
	if r := pgweir(t, "verify", "--proof", proof, "--pubkey", pub); r.code != 1 {
		t.Fatalf("tampered record verified: %s", r.stdout)
	}
	if r := pgweir(t, "ledger", "verify-chain", "--dsn", main, "--sths", exp, "--pubkey", pub); r.code != 1 || !strings.Contains(r.stdout, "does not match its leaf hash") {
		t.Fatalf("verify-chain accepted an altered record: %s", r.stdout)
	}
	if r := pgweir(t, "ledger", "export", "--dsn", main, "--dir", t.TempDir()); r.code == 0 || !strings.Contains(r.stderr, "does not match its leaf hash") {
		t.Fatalf("export of an altered record: exit %d %s", r.code, r.stderr)
	}

	// Tamper with a leaf hash: verify-chain against the exported tree heads
	// fails. With the stored compact range gone, the next merge has to
	// rebuild the tree from the leaves and refuses to extend it.
	pgtest.Exec(t, main, `UPDATE pgweir.ledger_leaves SET leaf_hash = sha256('x') WHERE leaf_index = 0`)
	if r := pgweir(t, "ledger", "verify-chain", "--dsn", main, "--sths", exp, "--pubkey", pub); r.code != 1 || !strings.HasPrefix(r.stdout, "invalid:") {
		t.Fatalf("verify-chain accepted a rewritten ledger: %s", r.stdout)
	}
	pgtest.Exec(t, main, `DELETE FROM pgweir.config WHERE key = 'ledger_compact_range'`)
	c := createBranch(t, main, "after")
	pgtest.Exec(t, c, "UPDATE orders SET status = 'C' WHERE id = 3")
	if r := pgweir(t, mergeArgs(main, "after", key, "fail", "--yes")...); r.code == 0 || !strings.Contains(r.stderr, "altered") {
		t.Fatalf("merge extended a tampered ledger: exit %d\n%s", r.code, r.stderr)
	}
}

// TestManualProviderDumpRestoredFork uses a fork whose user-defined type
// OIDs differ from main's (as after pg_dump/restore): no SCHEMA_DRIFT.
func TestManualProviderDumpRestoredFork(t *testing.T) {
	key, _ := keygen(t)
	items := `
CREATE TYPE item_kind AS ENUM ('std', 'rush');
CREATE TABLE items (id int PRIMARY KEY, kind item_kind NOT NULL, v text);
INSERT INTO items SELECT g, 'std', 'v' || g FROM generate_series(1, 10) g;`
	main := srv.NewMain(t, pgtest.UniqueName("main"), items)
	mustPgweir(t, "init", "--dsn", main, "--tables", "public.items")
	fork := srv.NewMain(t, pgtest.UniqueName("fork"), "CREATE TYPE shifts_oids AS ENUM ('x');"+items+schemasql.Schema)
	if pgtest.Query(t, main, "SELECT 'item_kind'::regtype::oid::text") == pgtest.Query(t, fork, "SELECT 'item_kind'::regtype::oid::text") {
		t.Fatal("test setup: type OIDs should differ")
	}
	r := mustPgweir(t, "branch", "create", "manual", "--dsn", main, "--provider", "manual", "--branch-dsn", fork)
	if !strings.Contains(r.stderr, "cannot verify") {
		t.Errorf("manual provider warning missing:\n%s", r.stderr)
	}
	pgtest.Exec(t, fork, "UPDATE items SET kind = 'rush' WHERE id = 1")
	mustPgweir(t, mergeArgs(main, "manual", key, "fail", "--yes")...)
	if got := pgtest.Query(t, main, "SELECT kind::text FROM items WHERE id = 1"); got != "rush" {
		t.Fatalf("kind = %s", got)
	}
	// Dropping a manual branch leaves the fork in place.
	mustPgweir(t, "branch", "drop", "manual", "--dsn", main)
	if got := pgtest.Query(t, fork, "SELECT count(*)::text FROM items"); got != "10" {
		t.Fatalf("fork changed: %s", got)
	}
}
