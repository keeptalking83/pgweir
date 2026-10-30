// SPDX-License-Identifier: Apache-2.0

// Package merge is the three-way merge engine (spec §9). It is pure: no I/O.
package merge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/keeptalking83/pgweir/internal/changeset"
)

// Policy decides what happens to conflicting rows.
type Policy string

// Policies.
const (
	PolicyFail   Policy = "FAIL"
	PolicySkip   Policy = "SKIP"
	PolicyAccept Policy = "ACCEPT"
)

// ParsePolicy parses fail, skip or accept (case-insensitive).
func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(strings.ToUpper(strings.TrimSpace(s))); p {
	case PolicyFail, PolicySkip, PolicyAccept:
		return p, nil
	}
	return "", fmt.Errorf("unknown policy %q (want fail, skip or accept)", s)
}

// Kind is a conflict kind.
type Kind string

// Conflict kinds.
const (
	InsertInsert Kind = "INSERT_INSERT"
	DeleteModify Kind = "DELETE_MODIFY"
	ModifyDelete Kind = "MODIFY_DELETE"
	CellConflict Kind = "CELL_CONFLICT"
)

// Resolution records what the policy did with a conflict.
type Resolution string

// Resolutions.
const (
	Unresolved     Resolution = "UNRESOLVED"
	Skipped        Resolution = "SKIPPED"
	AcceptedTheirs Resolution = "ACCEPTED_THEIRS"
)

// ActionKind is the statement an action becomes on main.
type ActionKind string

// Action kinds.
const (
	Insert ActionKind = "INSERT"
	Update ActionKind = "UPDATE"
	Delete ActionKind = "DELETE"
)

// Plan statuses.
const (
	StatusComputed = "COMPUTED"
	StatusConflict = "CONFLICT"
)

// Action is one row change to apply to main. ExpectOurs is the exact main
// row the decision was based on (nil: the row must be absent); apply
// re-verifies it under FOR UPDATE.
type Action struct {
	Kind       ActionKind    `json:"kind"`
	Schema     string        `json:"schema"`
	Table      string        `json:"table"`
	PK         changeset.Row `json:"pk"`
	Row        changeset.Row `json:"row"`
	ExpectOurs changeset.Row `json:"expect_ours"`
	FirstSeq   int64         `json:"-"`
	LastSeq    int64         `json:"-"`
}

// Cell is one conflicting column of a CELL_CONFLICT.
type Cell struct {
	Column string  `json:"column"`
	Base   *string `json:"base"`
	Ours   *string `json:"ours"`
	Theirs *string `json:"theirs"`
}

// Conflict is a row both sides changed incompatibly.
type Conflict struct {
	Schema     string        `json:"schema"`
	Table      string        `json:"table"`
	PK         changeset.Row `json:"pk"`
	Kind       Kind          `json:"kind"`
	Cells      []Cell        `json:"cells"`
	Base       changeset.Row `json:"base"`
	Ours       changeset.Row `json:"ours"`
	Theirs     changeset.Row `json:"theirs"`
	Resolution Resolution    `json:"resolution"`
}

// Stats counts the outcome per row. Skipped counts rows that produce no
// action: no-ops (main already matches) and conflicts under SKIP or FAIL.
type Stats struct {
	RowsTotal int `json:"rows_total"`
	Inserted  int `json:"inserted"`
	Updated   int `json:"updated"`
	Deleted   int `json:"deleted"`
	Skipped   int `json:"skipped"`
	Conflicts int `json:"conflicts"`
}

// Plan is the result of a merge computation.
type Plan struct {
	Policy    Policy     `json:"policy"`
	Status    string     `json:"status"`
	Stats     Stats      `json:"stats"`
	Actions   []Action   `json:"actions"`
	Conflicts []Conflict `json:"conflicts"`
	Warnings  []string   `json:"warnings"`
}

// Input is everything the engine needs.
type Input struct {
	Rows   []changeset.RowChange
	Ours   map[string]changeset.Row // by OursKey; a missing key means the row is absent on main
	Policy Policy
	// SequencePK holds "schema.table" for tables whose primary key is
	// filled by a sequence.
	SequencePK map[string]bool
}

// OursKey is the key of a main row in Input.Ours.
func OursKey(schema, table string, pk changeset.Row) string {
	return schema + "\x00" + table + "\x00" + changeset.Key(pk)
}

// Compute merges every changed row of the branch against main.
func Compute(in Input) Plan {
	p := Plan{Policy: in.Policy, Status: StatusComputed, Actions: []Action{}, Conflicts: []Conflict{}, Warnings: []string{}}
	acceptedOnSequence := map[string]int{}
	for _, rc := range in.Rows {
		ours := in.Ours[OursKey(rc.Schema, rc.Table, rc.PK)]
		act, conflict := decide(rc, ours)
		if conflict != nil {
			switch in.Policy {
			case PolicyFail:
				conflict.Resolution = Unresolved
				act = nil
			case PolicySkip:
				conflict.Resolution = Skipped
				act = nil
			case PolicyAccept:
				conflict.Resolution = AcceptedTheirs
				act = acceptTheirs(rc, ours, conflict.Kind)
				if conflict.Kind == InsertInsert && in.SequencePK[rc.Schema+"."+rc.Table] {
					acceptedOnSequence[rc.Schema+"."+rc.Table]++
				}
			}
			p.Conflicts = append(p.Conflicts, *conflict)
		}
		if act == nil {
			p.Stats.Skipped++
			continue
		}
		act.FirstSeq, act.LastSeq = rc.FirstSeq, rc.LastSeq
		p.Actions = append(p.Actions, *act)
		switch act.Kind {
		case Insert:
			p.Stats.Inserted++
		case Update:
			p.Stats.Updated++
		case Delete:
			p.Stats.Deleted++
		}
	}
	p.Stats.RowsTotal = len(in.Rows)
	p.Stats.Conflicts = len(p.Conflicts)
	if in.Policy == PolicyFail && len(p.Conflicts) > 0 {
		p.Status = StatusConflict
	}
	tables := make([]string, 0, len(acceptedOnSequence))
	for t := range acceptedOnSequence {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%s: %d INSERT_INSERT conflict(s) on a sequence-generated key were resolved by overwriting main's row; "+
				"rows that drew the same key from copies of one sequence usually describe unrelated entities", t, acceptedOnSequence[t]))
	}
	return p
}

// decide applies the decision table of spec §9.2. It returns the action for
// a non-conflicting row (nil for a no-op), or the conflict.
func decide(rc changeset.RowChange, ours changeset.Row) (*Action, *Conflict) {
	b, t, o := rc.Base, rc.Final, ours
	switch {
	case b == nil && t != nil:
		switch {
		case o == nil:
			return action(Insert, rc, t, nil), nil
		case o.Equal(t):
			return nil, nil
		}
		return nil, conflict(rc, o, InsertInsert, nil)
	case b != nil && t == nil:
		switch {
		case o == nil:
			return nil, nil // already gone
		case o.Equal(b):
			return action(Delete, rc, nil, o), nil
		}
		return nil, conflict(rc, o, DeleteModify, nil)
	case b != nil && t != nil:
		if o == nil {
			return nil, conflict(rc, o, ModifyDelete, nil)
		}
		if o.Equal(b) {
			return update(rc, t, o), nil
		}
		tc := changedColumns(b, t)
		var cells []Cell
		for _, c := range tc {
			if !changeset.ValueEqual(b[c], o[c]) && !changeset.ValueEqual(t[c], o[c]) {
				cells = append(cells, Cell{Column: c, Base: b[c], Ours: o[c], Theirs: t[c]})
			}
		}
		if len(cells) > 0 {
			return nil, conflict(rc, o, CellConflict, cells)
		}
		return update(rc, overlay(o, t, tc), o), nil
	}
	return nil, nil // inserted and deleted on the branch; Collapse drops these
}

// acceptTheirs resolves a conflict in favour of the branch (spec §9.2).
func acceptTheirs(rc changeset.RowChange, ours changeset.Row, k Kind) *Action {
	switch k {
	case InsertInsert:
		return update(rc, rc.Final, ours)
	case DeleteModify:
		return action(Delete, rc, nil, ours)
	case ModifyDelete:
		return action(Insert, rc, rc.Final, nil)
	case CellConflict:
		return update(rc, overlay(ours, rc.Final, changedColumns(rc.Base, rc.Final)), ours)
	}
	return nil
}

func action(k ActionKind, rc changeset.RowChange, row, ours changeset.Row) *Action {
	return &Action{Kind: k, Schema: rc.Schema, Table: rc.Table, PK: rc.PK, Row: row, ExpectOurs: ours}
}

// update returns an UPDATE to row, or nil when main already holds exactly
// that row (an UPDATE would change nothing).
func update(rc changeset.RowChange, row, ours changeset.Row) *Action {
	if row.Equal(ours) {
		return nil
	}
	return action(Update, rc, row, ours)
}

func conflict(rc changeset.RowChange, ours changeset.Row, k Kind, cells []Cell) *Conflict {
	if cells == nil {
		cells = []Cell{}
	}
	return &Conflict{Schema: rc.Schema, Table: rc.Table, PK: rc.PK, Kind: k, Cells: cells,
		Base: rc.Base, Ours: ours, Theirs: rc.Final}
}

// changedColumns returns the sorted columns where a and b differ.
func changedColumns(a, b changeset.Row) []string {
	seen := map[string]bool{}
	var cols []string
	for _, r := range []changeset.Row{a, b} {
		for c := range r {
			if !seen[c] {
				seen[c] = true
				if !changeset.ValueEqual(a[c], b[c]) {
					cols = append(cols, c)
				}
			}
		}
	}
	sort.Strings(cols)
	return cols
}

// overlay returns base with src's values for cols.
func overlay(base, src changeset.Row, cols []string) changeset.Row {
	out := base.Clone()
	for _, c := range cols {
		out[c] = src[c]
	}
	return out
}

// HasConflictsUnderFail reports whether the plan must not be applied.
func (p Plan) HasConflictsUnderFail() bool {
	return p.Status == StatusConflict
}
