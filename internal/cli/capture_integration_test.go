// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/capture"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/pgtest"
	"github.com/keeptalking83/pgweir/internal/schema"
)

func TestInitAndCheck(t *testing.T) {
	main := srv.NewMain(t, pgtest.UniqueName("main"), shopSchema+"CREATE TABLE nokey (x int);")
	r := mustPgweir(t, "check", "--dsn", main, "--tables", "public.orders")
	if !strings.Contains(r.stderr, "filled by sequence") {
		t.Errorf("check should warn about the identity key:\n%s", r.stderr)
	}
	r = pgweir(t, "init", "--dsn", main, "--tables", "public.orders,public.nokey")
	if r.code == 0 || !strings.Contains(r.stderr, "no PRIMARY KEY") {
		t.Fatalf("init must reject keyless tables, got exit %d:\n%s", r.code, r.stderr)
	}
	r = mustPgweir(t, "init", "--dsn", main, "--tables", "public.customers,public.orders")
	if !strings.Contains(r.stderr, "public.orders.id is filled by sequence") {
		t.Errorf("init should warn about the identity key:\n%s", r.stderr)
	}
	mustPgweir(t, "init", "--dsn", main, "--tables", "public.customers,public.orders") // idempotent
	mustPgweir(t, "check", "--dsn", main)
	// init never alters tracked tables on main.
	if got := pgtest.Query(t, main, "SELECT relreplident::text FROM pg_class WHERE oid = 'orders'::regclass"); got != "d" {
		t.Fatalf("replica identity on main changed to %q", got)
	}
}

func TestCaptureDiff(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "capture")

	// Main is untouched; the branch has FULL replica identity and the publication.
	if got := pgtest.Query(t, main, "SELECT relreplident::text FROM pg_class WHERE oid = 'orders'::regclass"); got != "d" {
		t.Fatalf("replica identity on main changed to %q", got)
	}
	if got := pgtest.Query(t, main, "SELECT count(*)::text FROM pg_publication"); got != "0" {
		t.Fatalf("main has %s publications", got)
	}
	if got := pgtest.Query(t, dsn, "SELECT relreplident::text FROM pg_class WHERE oid = 'orders'::regclass"); got != "f" {
		t.Fatalf("branch replica identity is %q", got)
	}
	// A database-level timezone on the branch must not leak into values.
	pgtest.Exec(t, dsn, "ALTER DATABASE "+pgtest.Query(t, dsn, "SELECT current_database()")+" SET timezone = 'Asia/Tokyo'")

	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"r1","tool_call_id":"c1","agent":"bot"}');
UPDATE orders SET status = 'SHIPPED' WHERE id IN (1, 2);
COMMIT;`)
	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"r1","tool_call_id":"c2","agent":"bot"}');
INSERT INTO orders (customer_id, status, amount) VALUES (1, 'NEW', 9.99);
DELETE FROM orders WHERE id = 10;
COMMIT;`)
	pgtest.Exec(t, dsn, `BEGIN;
SELECT pgweir.tag('{"run_id":"r2","parent_run_id":"r1","tool_call_id":"c3"}');
UPDATE orders SET status = 'DONE' WHERE id = 1;
UPDATE orders SET status = 'TOASTED' WHERE id = 20;
COMMIT;`)
	pgtest.Exec(t, dsn, "UPDATE customers SET tier = 'gold' WHERE id = 1") // untagged
	// Primary-key change: captured as delete + insert. Identity columns
	// cannot be updated, so use customers.
	pgtest.Exec(t, dsn, `BEGIN; SELECT pgweir.tag('{"run_id":"r3"}');
INSERT INTO customers VALUES (500, 'pk@example.com', 'basic');
UPDATE customers SET id = 501 WHERE id = 500;
UPDATE customers SET id = 600 WHERE id = 99;
COMMIT;`)

	cs := diffJSON(t, main, "capture")
	rows := map[string]int{}
	for i, r := range cs.Rows {
		rows[r.Table+"/"+val(r.PK, "id")] = i
	}
	get := func(key string) int {
		i, ok := rows[key]
		if !ok {
			t.Fatalf("row %s missing from changeset; rows: %v", key, rows)
		}
		return i
	}
	r := cs.Rows[get("orders/1")]
	if val(r.Base, "status") != "PENDING" || val(r.Final, "status") != "DONE" || strings.Join(r.Runs, ",") != "r1,r2" || r.Ops != 2 {
		t.Errorf("orders/1: %+v", r)
	}
	if !strings.HasSuffix(val(r.Base, "created_at"), "+00") {
		t.Errorf("timestamptz not rendered in UTC: %q", val(r.Base, "created_at"))
	}
	if val(r.Base, "flag") != "f" {
		t.Errorf("boolean must use the output function form, got %q", val(r.Base, "flag"))
	}
	if r := cs.Rows[get("orders/2")]; val(r.Final, "status") != "SHIPPED" || strings.Join(r.Runs, ",") != "r1" {
		t.Errorf("orders/2: %+v", r)
	}
	if r := cs.Rows[get("orders/101")]; r.Base != nil || val(r.Final, "amount") != "9.99" {
		t.Errorf("orders/101 insert: %+v", r)
	}
	if r := cs.Rows[get("orders/10")]; r.Final != nil || val(r.Base, "status") != "PENDING" {
		t.Errorf("orders/10 delete: %+v", r)
	}
	if r := cs.Rows[get("orders/20")]; val(r.Final, "note") != val(r.Base, "note") || len(val(r.Base, "note")) < 6000 || val(r.Final, "status") != "TOASTED" {
		t.Errorf("orders/20 TOAST: base note %d bytes, final %d bytes", len(val(r.Base, "note")), len(val(r.Final, "note")))
	}
	if r := cs.Rows[get("customers/1")]; strings.Join(r.Runs, ",") != "untagged" || val(r.Final, "tier") != "gold" {
		t.Errorf("customers/1: %+v", r)
	}
	if _, ok := rows["customers/500"]; ok {
		t.Errorf("row inserted then re-keyed on the branch must not appear under its old key")
	}
	if r := cs.Rows[get("customers/501")]; r.Base != nil || val(r.Final, "email") != "pk@example.com" {
		t.Errorf("customers/501: %+v", r)
	}
	if r := cs.Rows[get("customers/99")]; r.Final != nil {
		t.Errorf("customers/99 should be deleted (re-keyed): %+v", r)
	}
	if r := cs.Rows[get("customers/600")]; r.Base != nil || val(r.Final, "email") != "c99@example.com" {
		t.Errorf("customers/600: %+v", r)
	}
	var r1 map[string]any
	for _, run := range cs.Runs {
		if run["run_id"] == "r1" {
			r1 = run
		}
	}
	if r1 == nil || r1["tool_call_id"] != "c1" || r1["agent"] != "bot" {
		t.Errorf("run r1 metadata: %v", cs.Runs)
	}

	// A second diff captures nothing new and returns the same changeset.
	again := diffJSON(t, main, "capture")
	if len(again.Rows) != len(cs.Rows) {
		t.Fatalf("second diff has %d rows, first %d", len(again.Rows), len(cs.Rows))
	}

	// Human output.
	h := mustPgweir(t, "diff", "capture", "--dsn", main, "--idle-timeout", "300ms", "--table", "public.orders")
	if !strings.Contains(h.stderr, "status: PENDING -> DONE") || strings.Contains(h.stderr, "public.customers") {
		t.Errorf("human diff output:\n%s", h.stderr)
	}
}

// TestCaptureCrashRecovery kills capture after a transaction is persisted
// but before the slot is acknowledged, then reruns it: no loss, no duplicates.
func TestCaptureCrashRecovery(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "crash")
	for i := 1; i <= 3; i++ {
		pgtest.Exec(t, dsn, fmt.Sprintf(`BEGIN; SELECT pgweir.tag('{"run_id":"r"}'); UPDATE orders SET amount = amount + 1 WHERE id = %d; COMMIT;`, i))
	}

	ctx := context.Background()
	conn, err := pg.Connect(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	b, err := branch.Get(ctx, conn, "crash")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := branch.LoadConfig(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := schema.Load(ctx, conn, cfg.Refs())
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated crash")
	_, err = capture.Run(ctx, conn, b, tables, capture.Options{
		IdleTimeout:  300e6,
		AfterPersist: func(pglogrepl.LSN) error { return crash },
	})
	if !errors.Is(err, crash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	if n, _ := branch.EventCount(ctx, conn, b.ID); n != 1 {
		t.Fatalf("after crash: %d events persisted, want 1", n)
	}
	if _, err := capture.Run(ctx, conn, b, tables, capture.Options{IdleTimeout: 300e6}); err != nil {
		t.Fatal(err)
	}
	if n, _ := branch.EventCount(ctx, conn, b.ID); n != 3 {
		t.Fatalf("after rerun: %d events, want 3 (no loss, no duplicates)", n)
	}
	cs := diffJSON(t, main, "crash")
	if len(cs.Rows) != 3 {
		t.Fatalf("changeset has %d rows, want 3", len(cs.Rows))
	}
}

func TestBranchListAndDrop(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "dropme")
	db := pgtest.Query(t, dsn, "SELECT current_database()")
	r := mustPgweir(t, "branch", "list", "--dsn", main)
	if !strings.Contains(r.stdout, "dropme") || !strings.Contains(r.stdout, "OPEN") {
		t.Fatalf("branch list:\n%s", r.stdout)
	}
	// Not captured yet: drop must drain the slot and notice the change.
	pgtest.Exec(t, dsn, "UPDATE orders SET status = 'X' WHERE id = 1")
	if r := pgweir(t, "branch", "drop", "dropme", "--dsn", main, "--idle-timeout", "300ms"); r.code == 0 {
		t.Fatal("drop of a branch with unmerged changes must be refused")
	}
	mustPgweir(t, "branch", "drop", "dropme", "--dsn", main, "--force")
	if got := pgtest.Query(t, srv.AdminDSN("postgres"), "SELECT count(*)::text FROM pg_database WHERE datname = $1", db); got != "0" {
		t.Fatalf("fork database %s still exists", db)
	}
	if r := pgweir(t, "diff", "dropme", "--dsn", main); r.code == 0 || !strings.Contains(r.stderr, "DROPPED") {
		t.Fatalf("diff of a dropped branch must fail:\n%s", r.stderr)
	}
	// The name of a dropped branch can be used again.
	again := createBranch(t, main, "dropme")
	pgtest.Exec(t, again, "UPDATE orders SET status = 'Y' WHERE id = 2")
	cs := diffJSON(t, main, "dropme")
	if len(cs.Rows) != 1 || val(cs.Rows[0].Final, "status") != "Y" {
		t.Fatalf("recreated branch changeset: %+v", cs.Rows)
	}
	mustPgweir(t, "branch", "drop", "dropme", "--dsn", main, "--force")
}

func TestMainSchemaChangeAfterCapture(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "mainddl")
	pgtest.Exec(t, dsn, "UPDATE orders SET status = 'X' WHERE id = 1; DELETE FROM orders WHERE id = 2;")
	mustPgweir(t, "diff", "mainddl", "--dsn", main, "--idle-timeout", "300ms")
	pgtest.Exec(t, main, "ALTER TABLE orders ADD COLUMN priority int")
	r := pgweir(t, "merge", "mainddl", "--dsn", main, "--policy", "skip", "--dry-run", "--idle-timeout", "300ms")
	if r.code == 0 || !strings.Contains(r.stderr, "SCHEMA_DRIFT") {
		t.Fatalf("expected SCHEMA_DRIFT, got exit %d:\n%s", r.code, r.stderr)
	}
}

func TestManualBranchCannotBeMain(t *testing.T) {
	main := setup(t, "")
	r := pgweir(t, "branch", "create", "self", "--dsn", main, "--provider", "manual", "--branch-dsn", main)
	if r.code == 0 || !strings.Contains(r.stderr, "reaches main itself") {
		t.Fatalf("expected refusal, got exit %d:\n%s", r.code, r.stderr)
	}
	if got := pgtest.Query(t, main, "SELECT relreplident::text FROM pg_class WHERE oid = 'orders'::regclass"); got != "d" {
		t.Fatalf("replica identity on main changed to %q", got)
	}
	if got := pgtest.Query(t, main, "SELECT (SELECT count(*) FROM pg_publication) + (SELECT count(*) FROM pg_replication_slots WHERE database = current_database())"); got != "0" {
		t.Fatalf("publication or slot created on main: %s", got)
	}
}

func TestTagRejectsMalformedTags(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "badtag")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, tag := range []string{`[1]`, `{"x":1}`, `{"run_id":""}`, `{"run_id":7}`, `{"run_id":"r","n":1e309}`} {
		if _, err := conn.Exec(ctx, "SELECT pgweir.tag($1::jsonb)", tag); err == nil || !strings.Contains(err.Error(), "pgweir.tag()") {
			t.Errorf("tag %s: expected rejection, got %v", tag, err)
		}
	}
	if _, err := conn.Exec(ctx, `SELECT pgweir.tag('{"run_id":"ok","n":12.5,"nested":{"k":[1,2]}}')`); err != nil {
		t.Fatalf("valid tag rejected: %v", err)
	}
}

func TestSchemaDriftAborts(t *testing.T) {
	main := setup(t, "")
	dsn := createBranch(t, main, "drift")
	pgtest.Exec(t, dsn, "ALTER TABLE customers ADD COLUMN extra text; UPDATE customers SET extra = 'x' WHERE id = 1;")
	r := pgweir(t, "diff", "drift", "--dsn", main, "--idle-timeout", "300ms")
	if r.code == 0 || !strings.Contains(r.stderr, "SCHEMA_DRIFT") {
		t.Fatalf("expected SCHEMA_DRIFT, got exit %d:\n%s", r.code, r.stderr)
	}
}

func TestTruncateAborts(t *testing.T) {
	main := setup(t, "CREATE TABLE scratch (id int PRIMARY KEY);")
	mustPgweir(t, "init", "--dsn", main, "--tables", "public.customers,public.orders,public.scratch")
	dsn := createBranch(t, main, "trunc")
	pgtest.Exec(t, dsn, "INSERT INTO scratch VALUES (1); TRUNCATE scratch;")
	r := pgweir(t, "diff", "trunc", "--dsn", main, "--idle-timeout", "300ms")
	if r.code == 0 || !strings.Contains(r.stderr, "TRUNCATE_NOT_SUPPORTED") {
		t.Fatalf("expected TRUNCATE_NOT_SUPPORTED, got exit %d:\n%s", r.code, r.stderr)
	}
}

func TestLocalProviderNeedsCreateDB(t *testing.T) {
	role := pgtest.UniqueName("nocdb")
	db := pgtest.UniqueName("main")
	admin := srv.AdminDSN("postgres")
	pgtest.Exec(t, admin, "CREATE ROLE "+role+" LOGIN PASSWORD 'pw' REPLICATION")
	pgtest.Exec(t, admin, "CREATE DATABASE "+db+" OWNER "+role)
	main := srv.DSN(role, "pw", db)
	pgtest.Exec(t, main, "CREATE TABLE t (id int PRIMARY KEY)")
	mustPgweir(t, "init", "--dsn", main, "--tables", "public.t")
	r := mustPgweir(t, "check", "--dsn", main)
	if !strings.Contains(r.stderr, "lacks CREATEDB") {
		t.Errorf("check should report the missing CREATEDB:\n%s", r.stderr)
	}
	r = pgweir(t, "branch", "create", "x", "--dsn", main, "--provider", "local")
	if r.code == 0 || !strings.Contains(r.stderr, "CREATEDB") {
		t.Fatalf("expected an error naming CREATEDB, got exit %d:\n%s", r.code, r.stderr)
	}
}
