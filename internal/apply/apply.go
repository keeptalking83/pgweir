// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/ledger"
	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

const (
	statementsPerBatch = 500
	// maxSequenceCalls bounds sequence advancement (spec §10 step 8.4).
	maxSequenceCalls = 1_000_000
)

// Recorder appends the merge record to the ledger inside the apply
// transaction.
type Recorder interface {
	Append(ctx context.Context, tx pgx.Tx, rec ledger.Record) (*ledger.Appended, error)
}

// Merge identifies the merge being applied.
type Merge struct {
	ID            string
	Branch        *branch.Branch
	Changeset     *changeset.Changeset
	ChangesetHash []byte
	Plan          merge.Plan
}

// Options configure Apply.
type Options struct {
	Recorder Recorder
	Log      *slog.Logger
	// AfterVerify runs inside the transaction after the FOR UPDATE
	// re-verification. Tests use it to race a concurrent writer.
	AfterVerify func(ctx context.Context) error
}

// Result describes an applied merge.
type Result struct {
	MainLSNAfter string
	Ledger       *ledger.Appended
	Warnings     []string
}

// RecordComputed inserts the merges row and its conflicts (spec §10 step 5).
func RecordComputed(ctx context.Context, q pg.Querier, m *Merge, dryRun bool) error {
	stats, err := json.Marshal(m.Plan.Stats)
	if err != nil {
		return err
	}
	tables, err := json.Marshal(m.Changeset.Tables)
	if err != nil {
		return err
	}
	tx, err := beginOn(ctx, q)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
INSERT INTO pgweir.merges (id, branch_id, policy, dry_run, status, end_lsn, changeset_hash, changeset_tables, stats)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::pg_lsn, $7, $8::jsonb, $9::jsonb)`,
		m.ID, m.Branch.ID, string(m.Plan.Policy), dryRun, m.Plan.Status, m.Changeset.EndLSN, m.ChangesetHash, string(tables), string(stats))
	if err != nil {
		return fmt.Errorf("record merge: %w", err)
	}
	for _, c := range m.Plan.Conflicts {
		pk, _ := json.Marshal(c.PK)
		cells, _ := json.Marshal(c.Cells)
		base, _ := json.Marshal(c.Base)
		ours, _ := json.Marshal(c.Ours)
		theirs, _ := json.Marshal(c.Theirs)
		_, err = tx.Exec(ctx, `
INSERT INTO pgweir.conflicts (merge_id, table_schema, table_name, pk, kind, cells, base_row, ours_row, theirs_row, resolution)
VALUES ($1::uuid, $2, $3, $4::jsonb, $5, $6::jsonb, NULLIF($7::jsonb, 'null'), NULLIF($8::jsonb, 'null'), NULLIF($9::jsonb, 'null'), $10)`,
			m.ID, c.Schema, c.Table, string(pk), string(c.Kind), string(cells), string(base), string(ours), string(theirs), string(c.Resolution))
		if err != nil {
			return fmt.Errorf("record conflict: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func beginOn(ctx context.Context, q pg.Querier) (pgx.Tx, error) {
	switch c := q.(type) {
	case *pgx.Conn:
		return c.Begin(ctx)
	case pgx.Tx:
		return c.Begin(ctx)
	}
	return nil, fmt.Errorf("cannot begin a transaction on %T", q)
}

// MarkFinished sets the final status of a merge outside the apply
// transaction (ABORTED after a rollback).
func MarkFinished(ctx context.Context, q pg.Querier, mergeID, status string) error {
	_, err := q.Exec(ctx, "UPDATE pgweir.merges SET status = $2, finished_at = now() WHERE id = $1::uuid", mergeID, status)
	return err
}

// Apply runs the apply transaction on main (spec §10 step 8). On any error
// the transaction is rolled back; the caller marks the merge ABORTED.
func Apply(ctx context.Context, conn *pgx.Conn, tables []*schema.Table, m *Merge, opts Options) (*Result, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Recorder == nil {
		return nil, errs.New(errs.NoSigningKey, "no ledger signer configured")
	}
	byRef := map[schema.Ref]*schema.Table{}
	refs := make([]schema.Ref, len(tables))
	for i, t := range tables {
		byRef[t.Ref] = t
		refs[i] = t.Ref
	}
	edges, err := schema.ForeignKeys(ctx, conn, refs)
	if err != nil {
		return nil, err
	}
	ordered := OrderActions(m.Plan.Actions, TableOrder(refs, edges))

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// 8.1 Lock the branch row so concurrent merges of one branch serialize.
	var status string
	if err := tx.QueryRow(ctx, "SELECT status FROM pgweir.branches WHERE id = $1::uuid FOR UPDATE", m.Branch.ID).Scan(&status); err != nil {
		return nil, fmt.Errorf("lock branch: %w", err)
	}
	if status != branch.StatusOpen {
		return nil, errs.New(errs.BranchState, "branch %q became %s while the merge was computed", m.Branch.Name, status)
	}
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
		return nil, err
	}

	// 8.2 Lock and re-verify every affected main row.
	if err := verify(ctx, tx, byRef, ordered); err != nil {
		return nil, err
	}
	if opts.AfterVerify != nil {
		if err := opts.AfterVerify(ctx); err != nil {
			return nil, err
		}
	}

	// 8.3 Execute actions.
	if err := execute(ctx, tx, byRef, ordered); err != nil {
		return nil, err
	}

	// 8.4 Advance key sequences past inserted keys.
	res := &Result{}
	warnings, err := advanceSequences(ctx, tx, byRef, ordered)
	if err != nil {
		return nil, err
	}
	res.Warnings = warnings

	// 8.5 Mark the merge in main's WAL for downstream consumers.
	note, _ := json.Marshal(map[string]string{"merge_id": m.ID, "branch": m.Branch.Name})
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_logical_emit_message(true, $1, $2::text)", meta.MergePrefix, string(note)); err != nil {
		return nil, fmt.Errorf("emit merge message: %w", err)
	}

	// 8.6
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.pg_current_wal_lsn()::text").Scan(&res.MainLSNAfter); err != nil {
		return nil, err
	}

	// 8.7 Ledger leaf and signed tree head, in this transaction.
	rec, err := ledgerRecord(m, res.MainLSNAfter)
	if err != nil {
		return nil, err
	}
	appended, err := opts.Recorder.Append(ctx, tx, rec)
	if err != nil {
		return nil, err
	}
	res.Ledger = appended

	// 8.8
	if _, err := tx.Exec(ctx, `
UPDATE pgweir.merges SET status = 'APPLIED', main_lsn_after = $2::pg_lsn, leaf_index = $3, finished_at = now()
WHERE id = $1::uuid`, m.ID, res.MainLSNAfter, int64(appended.LeafIndex)); err != nil {
		return nil, err
	}
	if err := branch.SetStatus(ctx, tx, m.Branch.ID, branch.StatusMerged); err != nil {
		return nil, err
	}
	// 8.9
	if err := tx.Commit(ctx); err != nil {
		return nil, classify(err, "commit", nil)
	}
	return res, nil
}

func ledgerRecord(m *Merge, mainLSNAfter string) (ledger.Record, error) {
	stats, err := json.Marshal(m.Plan.Stats)
	if err != nil {
		return ledger.Record{}, err
	}
	conflicts, err := changeset.Canonical(m.Plan.Conflicts)
	if err != nil {
		return ledger.Record{}, err
	}
	ch := sha256.Sum256(conflicts)
	return ledger.Record{
		MergeID: m.ID, BranchID: m.Branch.ID, BranchName: m.Branch.Name,
		BaseLSN: m.Changeset.BaseLSN, EndLSN: m.Changeset.EndLSN, MainLSNAfter: mainLSNAfter,
		ChangesetHash: hex.EncodeToString(m.ChangesetHash), Policy: string(m.Plan.Policy),
		Stats: stats, ConflictsHash: hex.EncodeToString(ch[:]), Runs: m.Changeset.Runs,
	}, nil
}

func refOf(a merge.Action) schema.Ref { return schema.Ref{Schema: a.Schema, Name: a.Table} }

// verify locks every affected row with FOR UPDATE and compares it with the
// row the plan was computed from. Any difference aborts with RETRY.
func verify(ctx context.Context, tx pgx.Tx, byRef map[schema.Ref]*schema.Table, actions []merge.Action) error {
	pks := map[schema.Ref][]changeset.Row{}
	var order []schema.Ref
	for _, a := range actions {
		r := refOf(a)
		if pks[r] == nil {
			order = append(order, r)
		}
		pks[r] = append(pks[r], a.PK)
	}
	current := map[string]changeset.Row{}
	for _, r := range order {
		t, ok := byRef[r]
		if !ok {
			return fmt.Errorf("plan references untracked table %s", r)
		}
		rows, err := FetchRows(ctx, tx, t, pks[r], true)
		if err != nil {
			return err
		}
		for k, v := range rows {
			current[k] = v
		}
	}
	var changed []string
	for _, a := range actions {
		if !current[merge.OursKey(a.Schema, a.Table, a.PK)].Equal(a.ExpectOurs) {
			changed = append(changed, fmt.Sprintf("%s.%s %s", a.Schema, a.Table, changeset.Key(a.PK)))
		}
	}
	if len(changed) > 0 {
		return errs.New(errs.Retry, "main changed after the plan was computed; rerun merge (changed rows: %s)", strings.Join(changed, ", "))
	}
	return nil
}

type statement struct {
	sql    string
	args   []any
	action merge.Action
}

func execute(ctx context.Context, tx pgx.Tx, byRef map[schema.Ref]*schema.Table, actions []merge.Action) error {
	stmts := make([]statement, 0, len(actions))
	for _, a := range actions {
		s, ok, err := build(byRef[refOf(a)], a)
		if err != nil {
			return err
		}
		if ok {
			stmts = append(stmts, s)
		}
	}
	for start := 0; start < len(stmts); start += statementsPerBatch {
		end := min(start+statementsPerBatch, len(stmts))
		batch := &pgx.Batch{}
		for _, s := range stmts[start:end] {
			batch.Queue(s.sql, s.args...)
		}
		br := tx.SendBatch(ctx, batch)
		for _, s := range stmts[start:end] {
			tag, err := br.Exec()
			if err != nil {
				_ = br.Close()
				return classify(err, describe(s.action), byRef[refOf(s.action)])
			}
			if s.action.Kind == merge.Update && tag.RowsAffected() != 1 {
				_ = br.Close()
				return errs.New(errs.Retry, "%s affected %d rows; rerun merge", describe(s.action), tag.RowsAffected())
			}
		}
		if err := br.Close(); err != nil {
			return classify(err, "apply batch", nil)
		}
	}
	return nil
}

func describe(a merge.Action) string {
	return fmt.Sprintf("%s on %s.%s %s", a.Kind, a.Schema, a.Table, changeset.Key(a.PK))
}

// classify maps Postgres errors raised while applying (spec §10 step 9): a
// unique violation on the target table's primary key means a row with that
// key appeared after verification (RETRY); everything else aborts.
func classify(err error, what string, t *schema.Table) error {
	if pgErr, ok := pg.PgError(err); ok {
		if t != nil && pgErr.Code == pg.UniqueViolation && pgErr.ConstraintName == t.PKConstraint && t.PKConstraint != "" {
			return errs.Wrap(errs.Retry, err, "%s: a row with this key appeared on main after verification; rerun merge", what)
		}
		return errs.Wrap(errs.PGAborted, err, "%s rejected by Postgres; the merge was rolled back", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// build returns the SQL for an action. Values are bound as text and cast
// to the column type; NULL is SQL NULL. Generated columns are never written.
func build(t *schema.Table, a merge.Action) (statement, bool, error) {
	if t == nil {
		return statement{}, false, fmt.Errorf("plan references untracked table %s.%s", a.Schema, a.Table)
	}
	var args []any
	param := func(v *string, typ string) string {
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		return fmt.Sprintf("$%d::text::%s", len(args), typ)
	}
	where := func() string {
		parts := make([]string, len(t.PK))
		for i, k := range t.PK {
			c, _ := t.Column(k)
			parts[i] = pg.Ident(k) + " = " + param(a.PK[k], c.Type)
		}
		return strings.Join(parts, " AND ")
	}
	switch a.Kind {
	case merge.Insert:
		cols := make([]string, 0, len(t.Columns))
		vals := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			v, ok := a.Row[c.Name]
			if !ok {
				return statement{}, false, fmt.Errorf("insert into %s lacks column %s", t, c.Name)
			}
			cols = append(cols, pg.Ident(c.Name))
			vals = append(vals, param(v, c.Type))
		}
		overriding := ""
		if t.HasAlwaysIdentity() {
			overriding = " OVERRIDING SYSTEM VALUE"
		}
		sql := fmt.Sprintf("INSERT INTO %s (%s)%s VALUES (%s)", t.Qualified(), strings.Join(cols, ", "), overriding, strings.Join(vals, ", "))
		return statement{sql: sql, args: args, action: a}, true, nil
	case merge.Update:
		var sets []string
		for _, c := range t.Columns {
			if t.IsPK(c.Name) {
				continue
			}
			v := a.Row[c.Name]
			if changeset.ValueEqual(v, a.ExpectOurs[c.Name]) {
				continue
			}
			sets = append(sets, pg.Ident(c.Name)+" = "+param(v, c.Type))
		}
		if len(sets) == 0 {
			return statement{}, false, nil
		}
		sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s", t.Qualified(), strings.Join(sets, ", "), where())
		return statement{sql: sql, args: args, action: a}, true, nil
	case merge.Delete:
		sql := fmt.Sprintf("DELETE FROM %s WHERE %s", t.Qualified(), where())
		return statement{sql: sql, args: args, action: a}, true, nil
	}
	return statement{}, false, fmt.Errorf("unknown action %q", a.Kind)
}

// advanceSequences moves each key sequence past the largest key inserted
// by the merge, using nextval only, so it never moves backwards even with
// concurrent inserts on main (spec §10 step 8.4).
func advanceSequences(ctx context.Context, tx pgx.Tx, byRef map[schema.Ref]*schema.Table, actions []merge.Action) ([]string, error) {
	type target struct {
		max   int64
		found bool
	}
	maxima := map[schema.Ref]*target{}
	var refs []schema.Ref
	for _, a := range actions {
		if a.Kind != merge.Insert {
			continue
		}
		r := refOf(a)
		if maxima[r] == nil {
			maxima[r] = &target{}
			refs = append(refs, r)
		}
	}
	var warnings []string
	for _, r := range refs {
		t := byRef[r]
		seq, col, err := schema.KeySequence(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		if seq == "" {
			continue
		}
		tg := maxima[r]
		for _, a := range actions {
			if a.Kind != merge.Insert || refOf(a) != r || a.Row[col] == nil {
				continue
			}
			v, err := strconv.ParseInt(*a.Row[col], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("key %s.%s value %q is not an integer", t, col, *a.Row[col])
			}
			if !tg.found || v > tg.max {
				tg.max, tg.found = v, true
			}
		}
		if !tg.found {
			continue
		}
		var last, inc int64
		var called bool
		if err := tx.QueryRow(ctx, "SELECT last_value, is_called FROM "+seq).Scan(&last, &called); err != nil {
			return nil, fmt.Errorf("read sequence %s: %w", seq, err)
		}
		if err := tx.QueryRow(ctx, "SELECT seqincrement FROM pg_catalog.pg_sequence WHERE seqrelid = $1::regclass", seq).Scan(&inc); err != nil {
			return nil, fmt.Errorf("read sequence %s: %w", seq, err)
		}
		if inc <= 0 {
			warnings = append(warnings, fmt.Sprintf("sequence %s is descending and was not advanced", seq))
			continue
		}
		next := last // value the next nextval returns
		if called {
			next = last + inc
		}
		if next > tg.max {
			continue
		}
		calls := (tg.max-next)/inc + 1
		if calls > maxSequenceCalls {
			return nil, errs.New(errs.SequenceGapTooLarge,
				"advancing %s past %d would take %d nextval calls; run SELECT setval('%s', %d) during a quiet window, then rerun merge",
				seq, tg.max, calls, seq, tg.max)
		}
		if _, err := tx.Exec(ctx, "SELECT max(pg_catalog.nextval($1::regclass)) FROM generate_series(1, $2::bigint)", seq, calls); err != nil {
			return nil, fmt.Errorf("advance sequence %s: %w", seq, err)
		}
	}
	return warnings, nil
}
