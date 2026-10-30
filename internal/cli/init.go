// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

type tableFlags struct {
	tables    string
	allTables bool
	schemas   string
}

func (f *tableFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.tables, "tables", "", `tables to track, e.g. "public.orders,public.customers"`)
	cmd.Flags().BoolVar(&f.allTables, "all-tables", false, "track every ordinary table in --schemas")
	cmd.Flags().StringVar(&f.schemas, "schemas", "public", "schemas for --all-tables, comma separated")
}

func (f *tableFlags) resolve(ctx context.Context, q pg.Querier) ([]schema.Ref, error) {
	switch {
	case f.tables != "" && f.allTables:
		return nil, errs.New(errs.InvalidArgument, "use either --tables or --all-tables, not both")
	case f.tables != "":
		return schema.ParseRefs(f.tables)
	case f.allTables:
		var schemas []string
		for _, s := range strings.Split(f.schemas, ",") {
			if s = strings.TrimSpace(s); s != "" && s != meta.Schema {
				schemas = append(schemas, s)
			}
		}
		refs, err := schema.ListTables(ctx, q, schemas)
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			return nil, errs.New(errs.InvalidArgument, "no tables found in schemas %s", strings.Join(schemas, ", "))
		}
		return refs, nil
	}
	return nil, nil
}

func newInitCmd(a *app) *cobra.Command {
	var tf tableFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install the pgweir schema on main and choose the tracked tables",
		Long: `Install the pgweir metadata schema on main and record the tracked tables.

init never alters the tracked tables on main: REPLICA IDENTITY FULL and the
publication are set up on each branch by "branch create".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)

			refs, err := tf.resolve(ctx, conn)
			if err != nil {
				return err
			}
			if refs == nil {
				return errs.New(errs.InvalidArgument, "pass --tables or --all-tables")
			}
			info, err := pg.Server(ctx, conn)
			if err != nil {
				return err
			}
			if info.VersionNum < pg.MinServerVersion {
				return errs.New(errs.Prerequisite, "Postgres %s is not supported: version 14 or later is required", info.Version)
			}
			if e := info.EncodingProblem(); e != "" {
				return errs.New(errs.Prerequisite, "%s", e)
			}
			for _, p := range info.LogicalDecodingProblems() {
				if p != info.EncodingProblem() {
					a.say("warning: main's server: %s (branches on the same server or Neon project share this configuration)", p)
				}
			}
			tables, err := schema.Load(ctx, conn, refs)
			if err != nil {
				return errs.Wrap(errs.InvalidArgument, err, "cannot track tables")
			}
			var problems []string
			for _, t := range tables {
				for _, p := range t.Problems() {
					problems = append(problems, fmt.Sprintf("%s: %s", t, p))
				}
			}
			if len(problems) > 0 {
				return errs.New(errs.Prerequisite, "these tables cannot be tracked:\n  - %s", strings.Join(problems, "\n  - "))
			}
			if err := branch.Install(ctx, conn, tables); err != nil {
				return err
			}
			if err := warnSequenceKeys(ctx, a, conn, tables); err != nil {
				return err
			}
			names := make([]string, len(tables))
			for i, t := range tables {
				names[i] = t.String()
			}
			a.say("initialized %s on database %q, tracking %s: %s", meta.Name, info.Database, plural(len(tables), "table"), strings.Join(names, ", "))
			return nil
		},
	}
	tf.register(cmd)
	return cmd
}

// warnSequenceKeys prints the spec §4 warning for sequence-generated keys.
func warnSequenceKeys(ctx context.Context, a *app, conn *pgx.Conn, tables []*schema.Table) error {
	for _, t := range tables {
		seq, col, err := schema.KeySequence(ctx, conn, t)
		if err != nil {
			return err
		}
		if seq == "" {
			continue
		}
		a.say(`warning: %s.%s is filled by sequence %s.
  Branches start with a copy of that sequence, so rows inserted on main and on a
  branch can draw the same key. A merge advances main's sequence past the keys it
  inserts, but concurrent inserts on both sides still meet as INSERT_INSERT
  conflicts, and --policy accept would overwrite main's row. Prefer UUID or other
  globally unique keys for tables that both main and agents insert into.`, t, col, seq)
	}
	return nil
}
