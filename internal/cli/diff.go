// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/changeset"
)

const diffRowLimit = 50

func newDiffCmd(a *app) *cobra.Command {
	var out, table, run string
	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Capture the branch's changes and show the changeset",
		Long: `Capture the branch's committed changes and show the collapsed changeset:
for every changed row its value at fork time (base) and on the branch (final).

--json prints the canonical changeset on stdout; --out writes the same bytes to
a file. --table and --run filter what is shown.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			bc, err := loadBranchContext(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if err := requireOpen(bc.branch); err != nil {
				return err
			}
			cs, err := a.captureAndBuild(ctx, conn, bc)
			if err != nil {
				return err
			}
			cs = cs.Filter(table, run)
			canon, err := cs.Canonical()
			if err != nil {
				return err
			}
			if out != "" {
				if err := writeFile(out, canon); err != nil {
					return err
				}
			}
			if a.cfg.JSON {
				a.emit("%s", canon)
				return nil
			}
			a.printDiff(cs)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the canonical changeset JSON to this file")
	cmd.Flags().StringVar(&table, "table", "", "show only this table (schema.name)")
	cmd.Flags().StringVar(&run, "run", "", "show only rows written by this run_id")
	return cmd
}

func (a *app) printDiff(cs *changeset.Changeset) {
	a.say("branch %s: %s changed (base %s, end %s)", cs.BranchName, plural(len(cs.Changes), "row"), cs.BaseLSN, cs.EndLSN)
	type group struct {
		name    string
		changes []changeset.RowChange
	}
	var groups []*group
	byName := map[string]*group{}
	for _, c := range cs.Changes {
		name := c.Schema + "." + c.Table
		g, ok := byName[name]
		if !ok {
			g = &group{name: name}
			byName[name] = g
			groups = append(groups, g)
		}
		g.changes = append(g.changes, c)
	}
	for _, g := range groups {
		counts := map[string]int{}
		for _, c := range g.changes {
			counts[c.Op()]++
		}
		a.say("")
		a.say("%s  inserted %d  updated %d  deleted %d", g.name, counts["I"], counts["U"], counts["D"])
		sort.SliceStable(g.changes, func(i, j int) bool { return pkLess(g.changes[i].PK, g.changes[j].PK) })
		for i, c := range g.changes {
			if i == diffRowLimit {
				a.say("  ... %d more", len(g.changes)-diffRowLimit)
				break
			}
			a.say("  %s | %s | %s", formatPK(c.PK), c.Op(), describeChange(c))
		}
	}
}

// pkLess orders keys for display: column by column, numerically when both
// values are integers. The canonical changeset keeps its own order.
func pkLess(a, b changeset.Row) bool {
	for _, col := range a.Columns() {
		x, y := rawValue(a[col]), rawValue(b[col])
		if x == y {
			continue
		}
		xi, errX := strconv.ParseInt(x, 10, 64)
		yi, errY := strconv.ParseInt(y, 10, 64)
		if errX == nil && errY == nil {
			return xi < yi
		}
		return x < y
	}
	return false
}

func formatPK(pk changeset.Row) string {
	cols := pk.Columns()
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = c + "=" + formatValue(pk[c])
	}
	return strings.Join(parts, ",")
}

func rawValue(v *string) string {
	if v == nil {
		return "NULL"
	}
	return *v
}

func formatValue(v *string) string {
	if v == nil {
		return "NULL"
	}
	s := *v
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	return s
}

// describeChange lists changed columns as "col: base -> final".
func describeChange(c changeset.RowChange) string {
	var parts []string
	switch c.Op() {
	case changeset.OpInsert:
		for _, col := range c.Final.Columns() {
			if _, isPK := c.PK[col]; isPK {
				continue
			}
			parts = append(parts, col+"="+formatValue(c.Final[col]))
		}
	case changeset.OpDelete:
		return "deleted"
	default:
		for _, col := range c.Final.Columns() {
			if !changeset.ValueEqual(c.Base[col], c.Final[col]) {
				parts = append(parts, fmt.Sprintf("%s: %s -> %s", col, formatValue(c.Base[col]), formatValue(c.Final[col])))
			}
		}
		if len(parts) == 0 {
			return "no net change"
		}
	}
	return strings.Join(parts, ", ")
}
