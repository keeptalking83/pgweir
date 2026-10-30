// SPDX-License-Identifier: Apache-2.0

package changeset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// LoadEvents reads the captured events of a branch with commit LSN up to
// endLSN, in seq order.
func LoadEvents(ctx context.Context, q pg.Querier, branchID, endLSN string) ([]Event, error) {
	rows, err := q.Query(ctx, `
SELECT seq, xid, commit_lsn::text, op, table_schema, table_name, pk, old_row, new_row, run
FROM pgweir.events WHERE branch_id = $1::uuid AND commit_lsn <= $2::pg_lsn ORDER BY seq`, branchID, endLSN)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var pk, oldRow, newRow, run []byte
		if err := rows.Scan(&e.Seq, &e.Xid, &e.CommitLSN, &e.Op, &e.Schema, &e.Table, &pk, &oldRow, &newRow, &run); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		if err := json.Unmarshal(pk, &e.PK); err != nil {
			return nil, fmt.Errorf("decode event %d pk: %w", e.Seq, err)
		}
		if oldRow != nil {
			if err := json.Unmarshal(oldRow, &e.Old); err != nil {
				return nil, fmt.Errorf("decode event %d old_row: %w", e.Seq, err)
			}
		}
		if newRow != nil {
			if err := json.Unmarshal(newRow, &e.New); err != nil {
				return nil, fmt.Errorf("decode event %d new_row: %w", e.Seq, err)
			}
		}
		e.Run = json.RawMessage(run)
		events = append(events, e)
	}
	return events, rows.Err()
}

// TableDefs converts introspected tables to changeset table definitions.
func TableDefs(tables []*schema.Table) []TableDef {
	defs := make([]TableDef, len(tables))
	for i, t := range tables {
		cols := make([]ColumnDef, len(t.Columns))
		for j, c := range t.Columns {
			cols[j] = ColumnDef{Name: c.Name, Type: c.Type}
		}
		defs[i] = TableDef{Schema: t.Schema, Name: t.Name, PK: t.PK, Columns: cols}
	}
	return defs
}
