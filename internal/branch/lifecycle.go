// SPDX-License-Identifier: Apache-2.0

package branch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/provider"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// CreateOptions configure Create.
type CreateOptions struct {
	Name     string
	Provider provider.Provider
	Log      *slog.Logger
}

// Create forks main and prepares the fork for capture (spec §6): it sets
// REPLICA IDENTITY FULL and creates the publication on the branch, then the
// replication slot, and only then records the branch. The caller reveals the
// DSN after Create returns.
func Create(ctx context.Context, mainDSN string, opts CreateOptions) (*Branch, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := ValidateName(opts.Name); err != nil {
		return nil, err
	}
	cfg, mainTables, err := prepare(ctx, mainDSN, opts.Name)
	if err != nil {
		return nil, err
	}

	// No connection to main is open while forking (local provider).
	log.Debug("forking main", "provider", opts.Provider.Name())
	branchDSN, ref, err := opts.Provider.Fork(ctx, mainDSN, opts.Name)
	if err != nil {
		return nil, err
	}
	slot := meta.SlotPrefix + opts.Name
	slotCreated := false
	cleanup := func(cause error) error {
		bg := context.WithoutCancel(ctx)
		if slotCreated {
			if err := dropSlot(bg, branchDSN, slot); err != nil {
				log.Warn("could not drop replication slot after failure", "slot", slot, "error", err)
			}
		}
		if provider.Owned(opts.Provider) {
			if err := opts.Provider.Drop(bg, ref); err != nil {
				log.Warn("could not drop fork after failure", "error", err)
			}
		}
		return cause
	}

	if err := ensureNotMain(ctx, mainDSN, branchDSN); err != nil {
		return nil, cleanup(err)
	}
	if err := prepareBranch(ctx, branchDSN, cfg, mainTables); err != nil {
		return nil, cleanup(err)
	}
	baseLSN, err := createSlot(ctx, branchDSN, slot)
	if err != nil {
		return nil, cleanup(err)
	}
	slotCreated = true

	b := &Branch{
		ID: NewID(), Name: opts.Name, Provider: opts.Provider.Name(), ProviderRef: ref,
		DSN: branchDSN, SlotName: slot, BaseLSN: baseLSN, Status: StatusOpen,
	}
	if err := register(ctx, mainDSN, b); err != nil {
		return nil, cleanup(err)
	}
	return b, nil
}

// prepare reads the configuration and main's table definitions, then closes
// its connection to main.
func prepare(ctx context.Context, mainDSN, name string) (*Config, []*schema.Table, error) {
	conn, err := pg.Connect(ctx, mainDSN)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	cfg, err := LoadConfig(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pgweir.branches WHERE name = $1 AND status <> 'DROPPED')", name).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if exists {
		return nil, nil, errs.New(errs.InvalidArgument, "branch %q already exists; drop it first or choose another name", name)
	}
	tables, err := schema.Load(ctx, conn, cfg.Refs())
	if err != nil {
		return nil, nil, err
	}
	return cfg, tables, nil
}

// ensureNotMain writes a random marker to main after the fork exists and
// checks that the fork does not see it. A fork made earlier cannot; main
// itself (a mistaken --branch-dsn) or a replica of main can.
func ensureNotMain(ctx context.Context, mainDSN, branchDSN string) error {
	marker := NewID()
	mainConn, err := pg.Connect(ctx, mainDSN)
	if err != nil {
		return err
	}
	defer func() { _ = mainConn.Close(context.WithoutCancel(ctx)) }()
	if _, err := mainConn.Exec(ctx, "INSERT INTO pgweir.config (key, value) VALUES ('fork_probe:' || $1, 'true')", marker); err != nil {
		return fmt.Errorf("write fork probe on main: %w", err)
	}
	defer func() {
		_, _ = mainConn.Exec(context.WithoutCancel(ctx), "DELETE FROM pgweir.config WHERE key = 'fork_probe:' || $1", marker)
	}()
	branchConn, err := pg.Connect(ctx, branchDSN)
	if err != nil {
		return err
	}
	defer func() { _ = branchConn.Close(context.WithoutCancel(ctx)) }()
	var seen bool
	err = branchConn.QueryRow(ctx, `SELECT pg_catalog.to_regclass('pgweir.config') IS NOT NULL
  AND EXISTS (SELECT 1 FROM pgweir.config WHERE key = 'fork_probe:' || $1)`, marker).Scan(&seen)
	if err != nil {
		// Without the pgweir schema the fork cannot be main; prepareBranch reports it.
		if pg.HasSQLState(err, pg.UndefinedTable) {
			return nil
		}
		return fmt.Errorf("read fork probe on the branch: %w", err)
	}
	if seen {
		return errs.New(errs.InvalidArgument, "the branch connection string reaches main itself (or a replica of main), not a fork")
	}
	return nil
}

// prepareBranch verifies the fork and sets it up for capture (spec §6 steps 3-4).
func prepareBranch(ctx context.Context, branchDSN string, cfg *Config, mainTables []*schema.Table) error {
	conn, err := pg.Connect(ctx, branchDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	info, err := pg.Server(ctx, conn)
	if err != nil {
		return err
	}
	if p := info.LogicalDecodingProblems(); len(p) > 0 {
		return errs.New(errs.Prerequisite, "the branch server cannot host a replication slot:\n  - %s", strings.Join(p, "\n  - "))
	}
	branchTables, err := schema.Load(ctx, conn, cfg.Refs())
	if err != nil {
		return errs.Wrap(errs.SchemaDrift, err, "tracked table missing on the branch")
	}
	for i, mt := range mainTables {
		if d := schema.Drift(mt, branchTables[i]); len(d) > 0 {
			return errs.New(errs.SchemaDrift, "table %s differs between main and branch:\n  - %s", mt, strings.Join(d, "\n  - "))
		}
	}
	var hasTag bool
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.to_regprocedure('pgweir.tag(jsonb)') IS NOT NULL").Scan(&hasTag); err != nil {
		return err
	}
	if !hasTag {
		return errs.New(errs.Prerequisite, "function pgweir.tag(jsonb) is missing on the branch: the fork must contain main's %s schema", meta.Schema)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	names := make([]string, len(branchTables))
	for i, t := range branchTables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+t.Qualified()+" REPLICA IDENTITY FULL"); err != nil {
			return fmt.Errorf("set REPLICA IDENTITY FULL on %s (branch): %w", t, err)
		}
		names[i] = t.Qualified()
	}
	existing, found, err := publicationTables(ctx, tx, meta.Publication)
	if err != nil {
		return err
	}
	if found {
		if !sameRefs(existing, cfg.Refs()) {
			return errs.New(errs.Prerequisite, "publication %q already exists on the branch and does not cover exactly the tracked tables", meta.Publication)
		}
	} else if _, err := tx.Exec(ctx, "CREATE PUBLICATION "+pg.Ident(meta.Publication)+" FOR TABLE "+strings.Join(names, ", ")); err != nil {
		return fmt.Errorf("create publication on the branch: %w", err)
	}
	return tx.Commit(ctx)
}

func publicationTables(ctx context.Context, q pg.Querier, pub string) ([]schema.Ref, bool, error) {
	var found bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = $1)", pub).Scan(&found); err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	rows, err := q.Query(ctx, "SELECT schemaname::text, tablename::text FROM pg_catalog.pg_publication_tables WHERE pubname = $1", pub)
	if err != nil {
		return nil, true, err
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (schema.Ref, error) {
		var r schema.Ref
		err := row.Scan(&r.Schema, &r.Name)
		return r, err
	})
	return refs, true, err
}

func sameRefs(a, b []schema.Ref) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(rs []schema.Ref) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.Schema + "\x00" + r.Name
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// createSlot creates the logical replication slot and returns its
// consistent point (spec §6 step 5).
func createSlot(ctx context.Context, branchDSN, slot string) (string, error) {
	conn, err := pg.ConnectReplication(ctx, branchDSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	res, err := pglogrepl.CreateReplicationSlot(ctx, conn, slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{
		Mode:           pglogrepl.LogicalReplication,
		SnapshotAction: "NOEXPORT_SNAPSHOT",
	})
	if err != nil {
		if pg.HasSQLState(err, pg.DuplicateObject) {
			return "", errs.Wrap(errs.Prerequisite, err, "replication slot %s already exists on the branch", slot)
		}
		return "", fmt.Errorf("create replication slot %s: %w", slot, err)
	}
	lsn, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		return "", fmt.Errorf("parse consistent point %q: %w", res.ConsistentPoint, err)
	}
	return lsn.String(), nil
}

func dropSlot(ctx context.Context, branchDSN, slot string) error {
	conn, err := pg.Connect(ctx, branchDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	_, err = conn.Exec(ctx, "SELECT pg_catalog.pg_drop_replication_slot(slot_name) FROM pg_catalog.pg_replication_slots WHERE slot_name = $1", slot)
	return err
}

func register(ctx context.Context, mainDSN string, b *Branch) error {
	conn, err := pg.Connect(ctx, mainDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	ref, err := json.Marshal(b.ProviderRef)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `
INSERT INTO pgweir.branches (id, name, provider, provider_ref, dsn, slot_name, base_lsn, status)
VALUES ($1::uuid, $2, $3, $4::jsonb, $5, $6, $7::pg_lsn, $8)
RETURNING created_at, updated_at`,
		b.ID, b.Name, b.Provider, string(ref), b.DSN, b.SlotName, b.BaseLSN, b.Status).Scan(&b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return fmt.Errorf("register branch: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO pgweir.capture_progress (branch_id, last_seq, flushed_lsn) VALUES ($1::uuid, 0, $2::pg_lsn)", b.ID, b.BaseLSN); err != nil {
		return fmt.Errorf("register branch: %w", err)
	}
	return tx.Commit(ctx)
}

// ErrUnmerged is returned by Drop for an open branch with captured events.
var ErrUnmerged = errors.New("branch has unmerged events")

// Drop drops the replication slot, then the fork, and marks the branch
// DROPPED. An OPEN branch with captured events is refused unless force.
func Drop(ctx context.Context, mainConn *pgx.Conn, b *Branch, p provider.Provider, force bool, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if b.Status == StatusDropped {
		return errs.New(errs.BranchState, "branch %q is already dropped", b.Name)
	}
	if b.Status == StatusOpen && !force {
		n, err := EventCount(ctx, mainConn, b.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return errs.Wrap(errs.BranchState, ErrUnmerged, "branch %q is OPEN with %d captured events; merge it or pass --force", b.Name, n)
		}
	}
	if err := dropSlot(ctx, b.DSN, b.SlotName); err != nil {
		if !force {
			return fmt.Errorf("drop replication slot %s (pass --force to continue without it): %w", b.SlotName, err)
		}
		log.Warn("could not drop replication slot", "slot", b.SlotName, "error", err)
	}
	if err := p.Drop(ctx, b.ProviderRef); err != nil {
		if !force {
			return fmt.Errorf("drop fork (pass --force to mark the branch dropped anyway): %w", err)
		}
		log.Warn("could not drop fork", "error", err)
	}
	return SetStatus(ctx, mainConn, b.ID, StatusDropped)
}
