// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
)

// Local forks main with CREATE DATABASE ... TEMPLATE on the same server
// (spec §5.1).
type Local struct {
	mainDSN string
	log     *slog.Logger
}

func (l *Local) Name() string { return "local" }

const (
	localAttempts = 5
	localBackoff  = time.Second
)

// maintenanceDatabases are tried in order; CREATE DATABASE must not run on a
// connection to main, which would itself block the copy.
var maintenanceDatabases = []string{"postgres", "template1"}

func connectMaintenance(ctx context.Context, mainDSN string) (*pgx.Conn, error) {
	var errList []error
	for _, db := range maintenanceDatabases {
		dsn, err := pg.WithDatabase(mainDSN, db)
		if err != nil {
			return nil, err
		}
		conn, err := pg.Connect(ctx, dsn)
		if err == nil {
			return conn, nil
		}
		errList = append(errList, err)
	}
	return nil, fmt.Errorf("connect to a maintenance database (%s): %w",
		strings.Join(maintenanceDatabases, ", "), errors.Join(errList...))
}

func (l *Local) Fork(ctx context.Context, mainDSN, name string) (string, map[string]any, error) {
	mainDB, err := pg.DatabaseName(mainDSN)
	if err != nil {
		return "", nil, err
	}
	target := meta.DBPrefix + name
	conn, err := connectMaintenance(ctx, mainDSN)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pg.Ident(target), pg.Ident(mainDB))
	for attempt := 1; ; attempt++ {
		_, err = conn.Exec(ctx, stmt)
		if err == nil {
			break
		}
		if pg.HasSQLState(err, pg.InsufficientPrivilege) {
			return "", nil, errs.Wrap(errs.Prerequisite, err,
				"cannot fork %q with the local provider: the role needs CREATEDB and must own the database", mainDB)
		}
		if !pg.HasSQLState(err, pg.ObjectInUse) {
			return "", nil, fmt.Errorf("create database %s: %w", target, err)
		}
		if attempt >= localAttempts {
			return "", nil, errs.Wrap(errs.Prerequisite, err,
				"database %q is in use; the local provider needs no other sessions on it while forking%s",
				mainDB, blockers(ctx, conn, mainDB))
		}
		l.log.Debug("main database busy, retrying fork", "attempt", attempt)
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(localBackoff):
		}
	}
	branchDSN, err := pg.WithDatabase(mainDSN, target)
	if err != nil {
		return "", nil, err
	}
	return branchDSN, map[string]any{"dbname": target}, nil
}

func blockers(ctx context.Context, conn *pgx.Conn, db string) string {
	rows, err := conn.Query(ctx, `
SELECT pid, coalesce(usename::text, ''), coalesce(application_name, ''), coalesce(client_addr::text, 'local'), coalesce(state, '')
FROM pg_catalog.pg_stat_activity
WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid()
ORDER BY pid`, db)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var pid int
		var user, app, addr, state string
		if rows.Scan(&pid, &user, &app, &addr, &state) == nil {
			lines = append(lines, fmt.Sprintf("  pid %d user=%s application=%q client=%s state=%s", pid, user, app, addr, state))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "; blocking sessions:\n" + strings.Join(lines, "\n")
}

func (l *Local) Drop(ctx context.Context, ref map[string]any) error {
	target, _ := ref["dbname"].(string)
	if !strings.HasPrefix(target, meta.DBPrefix) || len(target) <= len(meta.DBPrefix) {
		return fmt.Errorf("refusing to drop database %q: not created by the local provider", target)
	}
	conn, err := connectMaintenance(ctx, l.mainDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pg.Ident(target))
	if pg.HasSQLState(err, pg.ObjectInUse) {
		// Terminate remaining sessions (agents still connected to the branch).
		_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pg.Ident(target)+" WITH (FORCE)")
	}
	if err != nil {
		return fmt.Errorf("drop database %s: %w", target, err)
	}
	return nil
}
