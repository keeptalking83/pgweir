// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/keeptalking83/pgweir/internal/changeset"
)

// planJSON is the stable, canonical form written by `merge --out`. It
// contains no merge id or timestamps, so recomputing the same merge yields
// byte-identical output.
type planJSON struct {
	Version       int        `json:"version"`
	Branch        string     `json:"branch"`
	ChangesetHash string     `json:"changeset_hash"`
	Policy        Policy     `json:"policy"`
	Status        string     `json:"status"`
	Stats         Stats      `json:"stats"`
	Actions       []Action   `json:"actions"`
	Conflicts     []Conflict `json:"conflicts"`
	Warnings      []string   `json:"warnings"`
}

// CanonicalPlan returns the canonical JSON of a plan.
func CanonicalPlan(p Plan, branch, changesetHashHex string) ([]byte, error) {
	return changeset.Canonical(planJSON{
		Version: 1, Branch: branch, ChangesetHash: changesetHashHex,
		Policy: p.Policy, Status: p.Status, Stats: p.Stats,
		Actions: p.Actions, Conflicts: p.Conflicts, Warnings: p.Warnings,
	})
}

// TableCounts summarizes actions and conflicts per table, for display.
type TableCounts struct {
	Table                                       string
	Inserted, Updated, Deleted, Conflicts, Skip int
}

// CountsByTable groups a plan by table, sorted by name.
func CountsByTable(p Plan) []TableCounts {
	m := map[string]*TableCounts{}
	get := func(schema, table string) *TableCounts {
		k := schema + "." + table
		if m[k] == nil {
			m[k] = &TableCounts{Table: k}
		}
		return m[k]
	}
	for _, a := range p.Actions {
		c := get(a.Schema, a.Table)
		switch a.Kind {
		case Insert:
			c.Inserted++
		case Update:
			c.Updated++
		case Delete:
			c.Deleted++
		}
	}
	for _, x := range p.Conflicts {
		get(x.Schema, x.Table).Conflicts++
	}
	out := make([]TableCounts, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}

// Describe renders a conflict on one line for humans.
func (c Conflict) Describe() string {
	pkCols := c.PK.Columns()
	pk := make([]string, len(pkCols))
	for i, col := range pkCols {
		pk[i] = col + "=" + show(c.PK[col])
	}
	s := fmt.Sprintf("%s.%s %s %s", c.Schema, c.Table, strings.Join(pk, ","), c.Kind)
	if c.Kind == CellConflict {
		parts := make([]string, len(c.Cells))
		for i, cell := range c.Cells {
			parts[i] = fmt.Sprintf("%s: base %s, main %s, branch %s", cell.Column, show(cell.Base), show(cell.Ours), show(cell.Theirs))
		}
		s += " (" + strings.Join(parts, "; ") + ")"
	}
	return s + " [" + string(c.Resolution) + "]"
}

func show(v *string) string {
	if v == nil {
		return "NULL"
	}
	s := *v
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	return fmt.Sprintf("%q", s)
}
