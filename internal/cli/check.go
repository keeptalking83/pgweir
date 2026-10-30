// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

type checkItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail"`
}

func newCheckCmd(a *app) *cobra.Command {
	var tf tableFlags
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check server and table prerequisites without changing anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)

			var items []checkItem
			add := func(name, status, format string, args ...any) {
				items = append(items, checkItem{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
			}
			info, err := pg.Server(ctx, conn)
			if err != nil {
				return err
			}
			if info.VersionNum < pg.MinServerVersion {
				add("server", "fail", "Postgres %s: version 14 or later is required", info.Version)
			} else {
				add("server", "ok", "Postgres %s, database %q, role %q", info.Version, info.Database, info.Role)
			}
			add("session settings", "ok", "pinned session settings are in effect")
			if p := info.LogicalDecodingProblems(); len(p) > 0 {
				for _, x := range p {
					add("logical decoding", "warn", "%s (needed on the server that hosts branches)", x)
				}
			} else {
				add("logical decoding", "ok", "wal_level=logical, %d/%d replication slots used, max_wal_senders=%d",
					info.UsedSlots, info.MaxReplicationSlots, info.MaxWALSenders)
			}
			if p := info.LocalProviderProblems(); len(p) > 0 {
				for _, x := range p {
					add("local provider", "warn", "%s", x)
				}
			} else {
				add("local provider", "ok", "role can fork database %q with CREATE DATABASE ... TEMPLATE", info.Database)
			}

			refs, err := tf.resolve(ctx, conn)
			if err != nil {
				return err
			}
			if refs == nil {
				initialized, err := branch.Initialized(ctx, conn)
				if err != nil {
					return err
				}
				if initialized {
					cfg, err := branch.LoadConfig(ctx, conn)
					if err != nil {
						return err
					}
					refs = cfg.Refs()
					add("init", "ok", "pgweir is initialized, tracking %s", plural(len(refs), "table"))
				} else {
					add("init", "warn", "pgweir is not initialized; pass --tables to check tables before init")
				}
			}
			for _, r := range refs {
				tables, err := schema.Load(ctx, conn, []schema.Ref{r})
				if err != nil {
					add(r.String(), "fail", "%v", err)
					continue
				}
				t := tables[0]
				if p := t.Problems(); len(p) > 0 {
					for _, x := range p {
						add(t.String(), "fail", "%s", x)
					}
					continue
				}
				add(t.String(), "ok", "primary key (%s)", strings.Join(t.PK, ", "))
				seq, col, err := schema.KeySequence(ctx, conn, t)
				if err != nil {
					return err
				}
				if seq != "" {
					add(t.String(), "warn", "key column %s is filled by sequence %s: concurrent inserts on main and branches can collide", col, seq)
				}
			}

			failed := false
			for _, it := range items {
				if it.Status == "fail" {
					failed = true
				}
			}
			if a.cfg.JSON {
				if err := a.emitJSON(map[string]any{"ok": !failed, "checks": items}); err != nil {
					return err
				}
			} else {
				for _, it := range items {
					a.say("%-4s  %-18s %s", it.Status, it.Name, it.Detail)
				}
			}
			if failed {
				return errs.New(errs.Prerequisite, "some checks failed")
			}
			return nil
		},
	}
	tf.register(cmd)
	return cmd
}
