// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/capture"
	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

func (a *app) connectMain(ctx context.Context) (*pgx.Conn, error) {
	if err := a.cfg.RequireMainDSN(); err != nil {
		return nil, errs.Wrap(errs.InvalidArgument, err, "missing configuration")
	}
	return pg.Connect(ctx, a.cfg.MainDSN)
}

func closeConn(ctx context.Context, conn *pgx.Conn) {
	_ = conn.Close(context.WithoutCancel(ctx))
}

// branchContext is what diff and merge need about a branch.
type branchContext struct {
	cfg    *branch.Config
	branch *branch.Branch
	tables []*schema.Table
}

func loadBranchContext(ctx context.Context, conn *pgx.Conn, name string) (*branchContext, error) {
	cfg, err := branch.LoadConfig(ctx, conn)
	if err != nil {
		return nil, err
	}
	b, err := branch.Get(ctx, conn, name)
	if err != nil {
		return nil, err
	}
	tables, err := schema.Load(ctx, conn, cfg.Refs())
	if err != nil {
		return nil, err
	}
	return &branchContext{cfg: cfg, branch: b, tables: tables}, nil
}

func requireOpen(b *branch.Branch) error {
	if b.Status != branch.StatusOpen {
		return errs.New(errs.BranchState, "branch %q is %s; only OPEN branches can be diffed or merged (drop it and create a new one)", b.Name, b.Status)
	}
	return nil
}

// captureAndBuild drains the branch slot and builds the full changeset.
func (a *app) captureAndBuild(ctx context.Context, conn *pgx.Conn, bc *branchContext) (*changeset.Changeset, error) {
	res, err := capture.Run(ctx, conn, bc.branch, bc.tables, capture.Options{IdleTimeout: a.cfg.IdleTimeout, Log: a.log})
	if err != nil {
		return nil, err
	}
	a.log.Debug("captured", "transactions", res.Transactions, "events", res.Events, "end_lsn", res.FlushedLSN)
	events, err := changeset.LoadEvents(ctx, conn, bc.branch.ID, res.FlushedLSN.String())
	if err != nil {
		return nil, err
	}
	return changeset.Build(changeset.Meta{
		BranchID: bc.branch.ID, BranchName: bc.branch.Name,
		BaseLSN: bc.branch.BaseLSN, EndLSN: res.FlushedLSN.String(),
	}, changeset.TableDefs(bc.tables), events)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644) // #nosec G306 -- output files are meant to be shared
}

func (a *app) emitJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.emit("%s", b)
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
