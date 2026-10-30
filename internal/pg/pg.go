// SPDX-License-Identifier: Apache-2.0

// Package pg contains pgx connection helpers, DSN handling and server
// prerequisite checks.
package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
)

// SessionParams are sent as startup parameters on every connection pgweir
// opens (spec §1.7). Type output depends on them; pinning them makes the text
// representation of a value identical on main and on the branch.
var SessionParams = map[string]string{
	"TimeZone":           "UTC",
	"DateStyle":          "ISO, YMD",
	"IntervalStyle":      "postgres",
	"extra_float_digits": "1",
	"bytea_output":       "hex",
	"lc_monetary":        "C",
}

// Querier is satisfied by *pgx.Conn and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func applySessionParams(params map[string]string) {
	for k, v := range SessionParams {
		params[k] = v
	}
	if params["application_name"] == "" {
		params["application_name"] = meta.Name
	}
}

// Connect opens a normal connection with the pinned session settings and
// verifies that they are in effect.
func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	applySessionParams(cfg.RuntimeParams)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", Redact(dsn), err)
	}
	if err := verifySession(ctx, conn); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

func verifySession(ctx context.Context, conn *pgx.Conn) error {
	names := make([]string, 0, len(SessionParams))
	for k := range SessionParams {
		names = append(names, k)
	}
	sort.Strings(names)
	var bad []string
	for _, name := range names {
		var got string
		if err := conn.QueryRow(ctx, "SELECT pg_catalog.current_setting($1)", name).Scan(&got); err != nil {
			return fmt.Errorf("read setting %s: %w", name, err)
		}
		if !strings.EqualFold(got, SessionParams[name]) {
			bad = append(bad, fmt.Sprintf("%s=%q (want %q)", name, got, SessionParams[name]))
		}
	}
	if len(bad) > 0 {
		return errs.New(errs.SessionSettingsOverridden, "session settings differ from the pinned values: %s", strings.Join(bad, ", "))
	}
	return nil
}

// ConnectReplication opens a logical replication connection
// (replication=database) with the pinned session settings.
func ConnectReplication(ctx context.Context, dsn string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	applySessionParams(cfg.RuntimeParams)
	cfg.RuntimeParams["replication"] = "database"
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open replication connection to %s: %w", Redact(dsn), err)
	}
	return conn, nil
}

// Ident quotes an identifier.
func Ident(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// QualifiedName returns "schema"."name".
func QualifiedName(schema, name string) string {
	return pgx.Identifier{schema, name}.Sanitize()
}

// Literal quotes a string literal for the rare statements that cannot take
// parameters (DDL, replication commands).
func Literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// PgError returns the server error inside err, if any.
func PgError(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr, true
	}
	return nil, false
}

// HasSQLState reports whether err carries the given SQLSTATE.
func HasSQLState(err error, code string) bool {
	pgErr, ok := PgError(err)
	return ok && pgErr.Code == code
}

// SQLSTATE codes used by pgweir.
const (
	UniqueViolation       = "23505"
	ObjectInUse           = "55006"
	UndefinedObject       = "42704"
	UndefinedTable        = "42P01"
	DeadlockDetected      = "40P01"
	LockNotAvailable      = "55P03"
	DuplicateObject       = "42710"
	InsufficientPrivilege = "42501"
)
