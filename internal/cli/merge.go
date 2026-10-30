// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/apply"
	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/ledger"
	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/meta"
)

// mergeHooks lets integration tests race main between steps.
type mergeHooks struct {
	beforeApply func(ctx context.Context) error
	afterVerify func(ctx context.Context) error
}

var testHooks mergeHooks

const conflictListLimit = 50

func newMergeCmd(a *app) *cobra.Command {
	var policyStr, out string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "merge <name>",
		Short: "Merge the branch's changes into main and record the merge in the ledger",
		Long: `Capture the branch's changes, compute a row-level three-way merge against
main, and apply it in one transaction under the chosen conflict policy:

  fail    any conflict aborts the merge; nothing is applied (exit 5)
  skip    conflicting rows are left as they are on main; the rest is applied
  accept  the branch wins every conflict

An applied merge is recorded as a leaf of the signed ledger. The merge id is
printed on stdout.

Exit codes: 0 applied or dry run ok, 2 needs confirmation (use --yes),
3 main changed meanwhile (rerun), 4 rejected by Postgres, 5 conflicts under fail.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if policyStr == "" {
				return errs.New(errs.InvalidArgument, "pass --policy fail, skip or accept")
			}
			policy, err := merge.ParsePolicy(policyStr)
			if err != nil {
				return errs.Wrap(errs.InvalidArgument, err, "policy")
			}
			var signer *ledger.Signer
			if !dryRun {
				if signer, err = a.signer(); err != nil {
					return err
				}
			}
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
			hash, err := cs.Hash()
			if err != nil {
				return err
			}
			plan, err := apply.Compute(ctx, conn, bc.tables, cs, policy)
			if err != nil {
				return err
			}
			m := &apply.Merge{ID: branch.NewID(), Branch: bc.branch, Changeset: cs, ChangesetHash: hash, Plan: plan}
			if err := apply.RecordComputed(ctx, conn, m, dryRun); err != nil {
				return err
			}
			if out != "" {
				data, err := merge.CanonicalPlan(plan, bc.branch.Name, hex.EncodeToString(hash))
				if err != nil {
					return err
				}
				if err := writeFile(out, data); err != nil {
					return err
				}
			}
			a.printPlan(m)

			if plan.Status == merge.StatusConflict {
				if err := a.emitMergeResult(m, nil); err != nil {
					return err
				}
				return errs.New(errs.Conflicts, "%s under policy fail; nothing was applied (see `%s conflicts %s`)",
					plural(len(plan.Conflicts), "conflict"), meta.Name, m.ID)
			}
			if dryRun {
				a.say("dry run: nothing was applied")
				return a.emitMergeResult(m, nil)
			}
			if !yes {
				ok, err := a.confirm("Apply? [y/N] ")
				if err != nil {
					return err
				}
				if !ok {
					return errs.New(errs.NeedsConfirm, "not applied: confirm interactively or pass --yes")
				}
			}
			if testHooks.beforeApply != nil {
				if err := testHooks.beforeApply(ctx); err != nil {
					return err
				}
			}
			res, err := apply.Apply(ctx, conn, bc.tables, m, apply.Options{Recorder: signer, AfterVerify: testHooks.afterVerify})
			if err != nil {
				if markErr := apply.MarkFinished(context.WithoutCancel(ctx), conn, m.ID, "ABORTED"); markErr != nil {
					a.log.Warn("could not mark merge aborted", "error", markErr)
				}
				return err
			}
			for _, w := range res.Warnings {
				a.say("warning: %s", w)
			}
			l := res.Ledger
			a.say("applied merge %s: leaf %d, tree size %d, root %s", m.ID, l.LeafIndex, l.Head.STH.TreeSize, l.Head.STH.RootHash)
			a.say("branch %q is now MERGED; prove the merge with: %s prove %s --out proof.json", bc.branch.Name, meta.Name, m.ID)
			return a.emitMergeResult(m, res)
		},
	}
	cmd.Flags().StringVar(&policyStr, "policy", "", "conflict policy: fail, skip or accept")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute and show the plan without applying it (no signing key needed)")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply without asking for confirmation")
	cmd.Flags().StringVar(&out, "out", "", "write the canonical plan JSON to this file")
	return cmd
}

func (a *app) signer() (*ledger.Signer, error) {
	if a.cfg.SigningKey == "" {
		return nil, errs.New(errs.NoSigningKey,
			"the ledger requires a signing key: create one with `%s keygen --out %s.key`, then pass --signing-key or set %sSIGNING_KEY (--dry-run works without a key)",
			meta.Name, meta.Name, meta.EnvPrefix)
	}
	key, err := ledger.LoadPrivateKey(a.cfg.SigningKey)
	if err != nil {
		return nil, errs.Wrap(errs.NoSigningKey, err, "signing key")
	}
	return &ledger.Signer{Key: key}, nil
}

func (a *app) confirm(prompt string) (bool, error) {
	f, ok := a.stdin.(*os.File)
	if !ok {
		return false, errs.New(errs.NeedsConfirm, "confirmation needed: pass --yes")
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false, errs.New(errs.NeedsConfirm, "stdin is not a terminal: pass --yes to apply")
	}
	_, _ = io.WriteString(a.stderr, prompt)
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func (a *app) printPlan(m *apply.Merge) {
	p := m.Plan
	a.say("merge %s of branch %q, policy %s: %s: %d to insert, %d to update, %d to delete, %d skipped, %s",
		m.ID, m.Branch.Name, p.Policy, plural(p.Stats.RowsTotal, "changed row"),
		p.Stats.Inserted, p.Stats.Updated, p.Stats.Deleted, p.Stats.Skipped, plural(p.Stats.Conflicts, "conflict"))
	for _, c := range merge.CountsByTable(p) {
		a.say("  %-30s insert %d  update %d  delete %d  conflicts %d", c.Table, c.Inserted, c.Updated, c.Deleted, c.Conflicts)
	}
	if len(p.Conflicts) > 0 {
		a.say("conflicts:")
		for i, c := range p.Conflicts {
			if i == conflictListLimit {
				a.say("  ... %d more", len(p.Conflicts)-conflictListLimit)
				break
			}
			a.say("  %s", c.Describe())
		}
	}
	for _, w := range p.Warnings {
		a.say("warning: %s", w)
	}
}

func (a *app) emitMergeResult(m *apply.Merge, res *apply.Result) error {
	if !a.cfg.JSON {
		a.emit("%s", m.ID)
		return nil
	}
	status := m.Plan.Status
	out := map[string]any{
		"merge_id": m.ID, "branch": m.Branch.Name, "policy": m.Plan.Policy,
		"stats": m.Plan.Stats, "warnings": m.Plan.Warnings, "changeset_hash": hex.EncodeToString(m.ChangesetHash),
	}
	if res != nil {
		status = "APPLIED"
		out["main_lsn_after"] = res.MainLSNAfter
		out["leaf_index"] = res.Ledger.LeafIndex
		out["sth"] = res.Ledger.Head
	}
	out["status"] = status
	return a.emitJSON(out)
}

func newConflictsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts <merge-id>",
		Short: "Show the conflicts recorded for a merge",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			conflicts, err := loadConflicts(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if a.cfg.JSON {
				return a.emitJSON(conflicts)
			}
			if len(conflicts) == 0 {
				a.say("merge %s has no conflicts", args[0])
				return nil
			}
			for _, c := range conflicts {
				a.emit("%s", c.Describe())
			}
			return nil
		},
	}
}

func loadConflicts(ctx context.Context, conn *pgx.Conn, mergeID string) ([]merge.Conflict, error) {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pgweir.merges WHERE id::text = $1)", mergeID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errs.New(errs.InvalidArgument, "merge %s does not exist", mergeID)
	}
	rows, err := conn.Query(ctx, `
SELECT table_schema, table_name, pk, kind, coalesce(cells, '[]'::jsonb), base_row, ours_row, theirs_row, resolution
FROM pgweir.conflicts WHERE merge_id = $1::uuid ORDER BY table_schema, table_name, pk::text`, mergeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []merge.Conflict{}
	for rows.Next() {
		var c merge.Conflict
		var pk, cells, base, ours, theirs []byte
		var kind, res string
		if err := rows.Scan(&c.Schema, &c.Table, &pk, &kind, &cells, &base, &ours, &theirs, &res); err != nil {
			return nil, err
		}
		c.Kind, c.Resolution = merge.Kind(kind), merge.Resolution(res)
		for _, f := range []struct {
			raw []byte
			dst any
		}{{pk, &c.PK}, {cells, &c.Cells}, {base, &c.Base}, {ours, &c.Ours}, {theirs, &c.Theirs}} {
			if f.raw != nil {
				if err := json.Unmarshal(f.raw, f.dst); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
