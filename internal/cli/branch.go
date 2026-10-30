// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/capture"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/provider"
)

func newBranchCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch",
		Short: "Create, list and drop branches",
	}
	cmd.AddCommand(newBranchCreateCmd(a), newBranchListCmd(a), newBranchDropCmd(a))
	return cmd
}

func newBranchCreateCmd(a *app) *cobra.Command {
	var providerName, branchDSN, parent string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Fork main into a branch and start capturing its changes",
		Long: `Fork main into a branch and prepare it for capture.

The branch connection string is printed on stdout and nothing else, so it can be
captured by scripts: DSN=$(pgweir branch create demo --provider local)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.cfg.RequireMainDSN(); err != nil {
				return errs.Wrap(errs.InvalidArgument, err, "missing configuration")
			}
			if providerName == "" {
				return errs.New(errs.InvalidArgument, "pass --provider (local, neon or manual)")
			}
			if branchDSN != "" && providerName != "manual" {
				return errs.New(errs.InvalidArgument, "--branch-dsn is only used with --provider manual")
			}
			p, err := provider.New(providerName, provider.Options{
				MainDSN: a.cfg.MainDSN, BranchDSN: branchDSN, NeonParent: parent, Log: a.log,
			})
			if err != nil {
				return errs.Wrap(errs.InvalidArgument, err, "provider")
			}
			if providerName == "manual" {
				a.say("warning: pgweir cannot verify that nothing was written to the manual fork before its replication slot was created; base rows are taken from the first change captured for each row")
			}
			b, err := branch.Create(ctx, a.cfg.MainDSN, branch.CreateOptions{Name: args[0], Provider: p, Log: a.log})
			if err != nil {
				return err
			}
			if a.cfg.JSON {
				return a.emitJSON(map[string]any{
					"id": b.ID, "name": b.Name, "provider": b.Provider, "dsn": b.DSN,
					"slot_name": b.SlotName, "base_lsn": b.BaseLSN, "status": b.Status,
				})
			}
			a.emit("%s", b.DSN)
			a.say("created branch %q (provider %s, base LSN %s); give the connection string above to the agent", b.Name, b.Provider, b.BaseLSN)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "fork provider: local, neon or manual")
	cmd.Flags().StringVar(&branchDSN, "branch-dsn", "", "connection string of a fork you created yourself (manual provider)")
	cmd.Flags().StringVar(&parent, "parent", "", "Neon parent branch ID or name (default: the project's default branch)")
	return cmd
}

func newBranchListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List branches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			if _, err := branch.LoadConfig(ctx, conn); err != nil {
				return err
			}
			branches, err := branch.List(ctx, conn)
			if err != nil {
				return err
			}
			if a.cfg.JSON {
				if branches == nil {
					branches = []*branch.Branch{}
				}
				return a.emitJSON(branches)
			}
			if len(branches) == 0 {
				a.say("no branches")
				return nil
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tPROVIDER\tBASE LSN\tCREATED")
			for _, b := range branches {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Name, b.Status, b.Provider, b.BaseLSN, b.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"))
			}
			return tw.Flush()
		},
	}
}

func newBranchDropCmd(a *app) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "drop <name>",
		Short: "Drop a branch: its replication slot, then the fork",
		Long: `Drop a branch: its replication slot first, then the fork.

An OPEN branch with captured, unmerged changes is refused unless --force.
Forks made with the manual provider are left in place; only the slot is dropped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			b, err := branch.Get(ctx, conn, args[0])
			if err != nil {
				return err
			}
			p, err := provider.New(b.Provider, provider.Options{MainDSN: a.cfg.MainDSN, Log: a.log})
			if err != nil {
				return err
			}
			if b.Status == branch.StatusOpen && !force {
				// Drain the slot first: work the agent did since the last
				// diff is not in pgweir.events yet.
				bc, err := loadBranchContext(ctx, conn, b.Name)
				if err != nil {
					return err
				}
				if _, err := capture.Run(ctx, conn, bc.branch, bc.tables, capture.Options{IdleTimeout: a.cfg.IdleTimeout, Log: a.log}); err != nil {
					return fmt.Errorf("cannot check branch %q for unmerged changes (pass --force to drop it anyway): %w", b.Name, err)
				}
			}
			if err := branch.Drop(ctx, conn, b, p, force, a.log); err != nil {
				return err
			}
			a.say("dropped branch %q (%s)", b.Name, pg.Redact(b.DSN))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "drop even if the branch has unmerged changes or cannot be reached")
	return cmd
}
