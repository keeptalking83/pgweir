// SPDX-License-Identifier: Apache-2.0

// Package apply reads main's current rows, computes merge plans and applies
// them to main in one transaction (spec §10).
package apply

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

const fetchBatch = 1000

// FetchRows returns main's current rows for the given primary keys, keyed by
// merge.OursKey. Absent rows are missing from the map.
//
// Values are read in the text result format, which is the type's output
// function under the pinned session settings: the same representation
// pgoutput sends for branch values. (A ::text cast is not always the output
// function: true::text is 'true' while boolout gives 't'.)
func FetchRows(ctx context.Context, q pg.Querier, t *schema.Table, pks []changeset.Row, forUpdate bool) (map[string]changeset.Row, error) {
	out := map[string]changeset.Row{}
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = pg.Ident(c.Name)
	}
	pkCols := make([]string, len(t.PK))
	unnest := make([]string, len(t.PK))
	for i, k := range t.PK {
		c, ok := t.Column(k)
		if !ok {
			return nil, fmt.Errorf("primary key column %s of %s is not a plain column", k, t)
		}
		pkCols[i] = pg.Ident(k)
		unnest[i] = fmt.Sprintf("$%d::text[]::%s[]", i+1, c.Type)
	}
	sql := fmt.Sprintf("SELECT %s FROM %s WHERE (%s) IN (SELECT * FROM unnest(%s))",
		strings.Join(cols, ", "), t.Qualified(), strings.Join(pkCols, ", "), strings.Join(unnest, ", "))
	if forUpdate {
		sql += " FOR UPDATE"
	}
	for start := 0; start < len(pks); start += fetchBatch {
		end := min(start+fetchBatch, len(pks))
		args := []any{pgx.QueryResultFormats{pgx.TextFormatCode}}
		for _, k := range t.PK {
			vals := make([]string, 0, end-start)
			for _, pk := range pks[start:end] {
				v := pk[k]
				if v == nil {
					return nil, fmt.Errorf("NULL primary key value in %s", t)
				}
				vals = append(vals, *v)
			}
			args = append(args, vals)
		}
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return nil, fmt.Errorf("read rows of %s: %w", t, err)
		}
		for rows.Next() {
			raw := rows.RawValues()
			r := make(changeset.Row, len(t.Columns))
			for i, c := range t.Columns {
				if raw[i] == nil {
					r[c.Name] = nil
				} else {
					s := string(raw[i])
					r[c.Name] = &s
				}
			}
			out[merge.OursKey(t.Schema, t.Name, r.Project(t.PK))] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read rows of %s: %w", t, err)
		}
	}
	return out, nil
}

// FetchOurs reads main's current row for every row of the changeset.
func FetchOurs(ctx context.Context, q pg.Querier, tables []*schema.Table, rows []changeset.RowChange) (map[string]changeset.Row, error) {
	byTable := map[schema.Ref][]changeset.Row{}
	for _, rc := range rows {
		ref := schema.Ref{Schema: rc.Schema, Name: rc.Table}
		byTable[ref] = append(byTable[ref], rc.PK)
	}
	ours := map[string]changeset.Row{}
	for _, t := range tables {
		pks := byTable[t.Ref]
		if len(pks) == 0 {
			continue
		}
		m, err := FetchRows(ctx, q, t, pks, false)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			ours[k] = v
		}
	}
	return ours, nil
}

// SequenceKeys returns "schema.table" for tables whose key is sequence-filled.
func SequenceKeys(ctx context.Context, q pg.Querier, tables []*schema.Table) (map[string]bool, error) {
	out := map[string]bool{}
	for _, t := range tables {
		seq, _, err := schema.KeySequence(ctx, q, t)
		if err != nil {
			return nil, err
		}
		if seq != "" {
			out[t.String()] = true
		}
	}
	return out, nil
}

// CheckColumns verifies that every captured row has exactly the columns of
// main's current table: a schema change after capture is drift.
func CheckColumns(tables []*schema.Table, rows []changeset.RowChange) error {
	want := map[schema.Ref]string{}
	for _, t := range tables {
		names := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			names[i] = c.Name
		}
		sort.Strings(names)
		want[t.Ref] = strings.Join(names, ",")
	}
	for _, rc := range rows {
		ref := schema.Ref{Schema: rc.Schema, Name: rc.Table}
		w, ok := want[ref]
		if !ok {
			return errs.New(errs.UntrackedTable, "captured change to %s, which is no longer tracked", ref)
		}
		for _, r := range []changeset.Row{rc.Base, rc.Final} {
			if r != nil && strings.Join(r.Columns(), ",") != w {
				return errs.New(errs.SchemaDrift, "table %s changed on main after the branch's changes were captured (columns now %s, captured %s)",
					ref, w, strings.Join(r.Columns(), ","))
			}
		}
	}
	return nil
}

// Compute fetches main's rows and computes the merge plan (spec §10 step 4).
func Compute(ctx context.Context, q pg.Querier, tables []*schema.Table, cs *changeset.Changeset, policy merge.Policy) (merge.Plan, error) {
	if err := CheckColumns(tables, cs.Changes); err != nil {
		return merge.Plan{}, err
	}
	ours, err := FetchOurs(ctx, q, tables, cs.Changes)
	if err != nil {
		return merge.Plan{}, err
	}
	seq, err := SequenceKeys(ctx, q, tables)
	if err != nil {
		return merge.Plan{}, err
	}
	return merge.Compute(merge.Input{Rows: cs.Changes, Ours: ours, Policy: policy, SequencePK: seq}), nil
}
