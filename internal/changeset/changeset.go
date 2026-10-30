// SPDX-License-Identifier: Apache-2.0

package changeset

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// Operations of an event.
const (
	OpInsert = "I"
	OpUpdate = "U"
	OpDelete = "D"
)

// Untagged is the run_id of writes made without a tag.
const Untagged = "untagged"

// Event is one captured row operation (a row of pgweir.events).
type Event struct {
	Seq       int64           `json:"seq"`
	Xid       int64           `json:"xid"`
	CommitLSN string          `json:"commit_lsn"`
	Op        string          `json:"op"`
	Schema    string          `json:"schema"`
	Table     string          `json:"table"`
	PK        Row             `json:"pk"`
	Old       Row             `json:"old"`
	New       Row             `json:"new"`
	Run       json.RawMessage `json:"run"`
}

// RunID returns the run_id of the event's tag.
func (e Event) RunID() string {
	var r struct {
		RunID string `json:"run_id"`
	}
	if json.Unmarshal(e.Run, &r) != nil || r.RunID == "" {
		return Untagged
	}
	return r.RunID
}

// RowChange is the collapsed history of one primary key on the branch
// (spec §8.1).
type RowChange struct {
	Schema        string `json:"schema"`
	Table         string `json:"table"`
	PK            Row    `json:"pk"`
	Base          Row    `json:"base"`
	Final         Row    `json:"final"`
	Runs          []string
	Ops           int
	FirstSeq      int64
	LastSeq       int64
	LastCommitLSN string
}

// Op classifies the net change: I (inserted on the branch), D (deleted) or U.
func (c RowChange) Op() string {
	switch {
	case c.Base == nil:
		return OpInsert
	case c.Final == nil:
		return OpDelete
	}
	return OpUpdate
}

// Collapse groups events by (schema, table, pk) in seq order. base is the
// old row of the first event, final the new row of the last event. Groups
// inserted and then deleted on the branch are dropped. The result is sorted
// by (schema, table, canonical pk JSON).
func Collapse(events []Event) []RowChange {
	sorted := make([]Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	type groupKey struct{ schema, table, pk string }
	groups := map[groupKey]*RowChange{}
	keys := map[groupKey]string{}
	var order []groupKey
	for _, e := range sorted {
		k := groupKey{e.Schema, e.Table, Key(e.PK)}
		g, ok := groups[k]
		if !ok {
			g = &RowChange{Schema: e.Schema, Table: e.Table, PK: e.PK, Base: e.Old, FirstSeq: e.Seq}
			groups[k] = g
			keys[k] = k.pk
			order = append(order, k)
		}
		g.Final = e.New
		g.Ops++
		g.LastSeq = e.Seq
		g.LastCommitLSN = e.CommitLSN
		run := e.RunID()
		if len(g.Runs) == 0 || !contains(g.Runs, run) {
			g.Runs = append(g.Runs, run)
		}
	}
	out := make([]RowChange, 0, len(order))
	for _, k := range order {
		g := groups[k]
		if g.Base == nil && g.Final == nil {
			continue
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return Key(a.PK) < Key(b.PK)
	})
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ColumnDef is a column of a changeset table.
type ColumnDef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TableDef describes a tracked table in the changeset.
type TableDef struct {
	Schema  string      `json:"schema"`
	Name    string      `json:"name"`
	PK      []string    `json:"pk"`
	Columns []ColumnDef `json:"columns"`
}

// RowJSON is a row of the canonical changeset.
type RowJSON struct {
	Schema string   `json:"schema"`
	Table  string   `json:"table"`
	PK     Row      `json:"pk"`
	Base   Row      `json:"base"`
	Final  Row      `json:"final"`
	Runs   []string `json:"runs"`
	Ops    int      `json:"ops"`
}

// Changeset is the canonical changeset of a branch (spec §8.2).
type Changeset struct {
	Version    int               `json:"version"`
	BranchID   string            `json:"branch_id"`
	BranchName string            `json:"branch_name"`
	BaseLSN    string            `json:"base_lsn"`
	EndLSN     string            `json:"end_lsn"`
	Tables     []TableDef        `json:"tables"`
	Rows       []RowJSON         `json:"rows"`
	Runs       []json.RawMessage `json:"runs"`

	// Changes keeps the collapsed rows with their seq bookkeeping; it is
	// not part of the canonical JSON.
	Changes []RowChange `json:"-"`
}

// Meta identifies the branch a changeset belongs to.
type Meta struct {
	BranchID   string
	BranchName string
	BaseLSN    string
	EndLSN     string
}

// Build collapses events into a changeset. tables is sorted by (schema, name).
func Build(m Meta, tables []TableDef, events []Event) (*Changeset, error) {
	tbl := make([]TableDef, len(tables))
	copy(tbl, tables)
	sort.Slice(tbl, func(i, j int) bool {
		if tbl[i].Schema != tbl[j].Schema {
			return tbl[i].Schema < tbl[j].Schema
		}
		return tbl[i].Name < tbl[j].Name
	})
	changes := Collapse(events)
	cs := &Changeset{
		Version: 1, BranchID: m.BranchID, BranchName: m.BranchName,
		BaseLSN: m.BaseLSN, EndLSN: m.EndLSN,
		Tables: tbl, Rows: make([]RowJSON, len(changes)), Changes: changes,
	}
	for i, c := range changes {
		cs.Rows[i] = RowJSON{Schema: c.Schema, Table: c.Table, PK: c.PK, Base: c.Base, Final: c.Final, Runs: c.Runs, Ops: c.Ops}
	}
	runs, err := collectRuns(events, changes)
	if err != nil {
		return nil, err
	}
	cs.Runs = runs
	return cs, nil
}

// collectRuns returns one run object per run_id referenced by the rows,
// sorted by run_id. For a run_id tagged with different metadata, the first
// tag (by seq) wins. The four standard fields are always present.
func collectRuns(events []Event, changes []RowChange) ([]json.RawMessage, error) {
	used := map[string]bool{}
	for _, c := range changes {
		for _, r := range c.Runs {
			used[r] = true
		}
	}
	sorted := make([]Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	first := map[string]json.RawMessage{}
	for _, e := range sorted {
		id := e.RunID()
		if !used[id] {
			continue
		}
		if _, ok := first[id]; !ok {
			first[id] = e.Run
		}
	}
	ids := make([]string, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]json.RawMessage, len(ids))
	for i, id := range ids {
		obj := map[string]any{}
		if len(first[id]) > 0 {
			if err := json.Unmarshal(first[id], &obj); err != nil {
				return nil, fmt.Errorf("decode run %q: %w", id, err)
			}
		}
		obj["run_id"] = id
		for _, k := range []string{"parent_run_id", "tool_call_id", "agent"} {
			if _, ok := obj[k]; !ok {
				obj[k] = nil
			}
		}
		b, err := Canonical(obj)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// Canonical returns the canonical JSON bytes of the changeset.
func (cs *Changeset) Canonical() ([]byte, error) {
	return Canonical(cs)
}

// Hash returns sha256 of the canonical changeset.
func (cs *Changeset) Hash() ([]byte, error) {
	b, err := cs.Canonical()
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

// Filter returns a copy restricted to one table and/or one run. Filtering
// is for display only; merges always use the full changeset.
func (cs *Changeset) Filter(table, run string) *Changeset {
	if table == "" && run == "" {
		return cs
	}
	out := *cs
	out.Rows = nil
	out.Changes = nil
	for i, c := range cs.Changes {
		if table != "" && c.Schema+"."+c.Table != table {
			continue
		}
		if run != "" && !contains(c.Runs, run) {
			continue
		}
		out.Rows = append(out.Rows, cs.Rows[i])
		out.Changes = append(out.Changes, c)
	}
	if out.Rows == nil {
		out.Rows = []RowJSON{}
	}
	return &out
}
