// SPDX-License-Identifier: Apache-2.0

//go:build integration

package pgtest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Role is the non-superuser role pgweir runs as in tests: it owns the
// databases and tables and has REPLICATION and CREATEDB, like a managed
// provider's application role.
const (
	Role     = "app"
	Password = "app"
)

// Server is a running Postgres container.
type Server struct {
	container *tcpostgres.PostgresContainer
	host      string
	port      string
	Image     string
}

var counter atomic.Int64

// Start runs a Postgres container with wal_level = logical.
func Start(ctx context.Context) (*Server, error) {
	image := os.Getenv("PGWEIR_TEST_PG_IMAGE")
	if image == "" {
		image = "postgres:16"
	}
	c, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithCmd("postgres",
			"-c", "fsync=off",
			"-c", "wal_level=logical",
			"-c", "max_replication_slots=40",
			"-c", "max_wal_senders=40",
			"-c", "max_connections=200"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", image, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	s := &Server{container: c, host: host, port: port.Port(), Image: image}
	if err := s.exec(ctx, s.DSN("postgres", "postgres", "postgres"), fmt.Sprintf(`
DO $$ BEGIN
  CREATE ROLE %s LOGIN PASSWORD '%s' REPLICATION CREATEDB;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$`, Role, Password)); err != nil {
		return nil, err
	}
	return s, nil
}

// Stop terminates the container.
func (s *Server) Stop(ctx context.Context) error {
	return testcontainers.TerminateContainer(s.container)
}

// DSN returns a URL connection string.
func (s *Server) DSN(user, password, db string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(s.host, s.port),
		Path:     "/" + db,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// AdminDSN connects as the superuser to the given database.
func (s *Server) AdminDSN(db string) string { return s.DSN("postgres", "postgres", db) }

func (s *Server) exec(ctx context.Context, dsn string, sql string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

// UniqueName returns a database name unique within the test process.
func UniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%100000, counter.Add(1))
}

// NewMain creates a database owned by Role, runs setup as Role in it, and
// returns Role's connection string.
func (s *Server) NewMain(t testing.TB, name, setup string) string {
	t.Helper()
	ctx := context.Background()
	if err := s.exec(ctx, s.AdminDSN("postgres"), fmt.Sprintf("CREATE DATABASE %s OWNER %s", name, Role)); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	dsn := s.DSN(Role, Password, name)
	if setup != "" {
		if err := s.exec(ctx, dsn, setup); err != nil {
			t.Fatalf("setup %s: %v", name, err)
		}
	}
	return dsn
}

// Exec runs statements on dsn and fails the test on error.
func Exec(t testing.TB, dsn string, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

// Query returns the first column of the first row as text.
func Query(t testing.TB, dsn string, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var v *string
	if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", firstLine(sql), err)
	}
	if v == nil {
		return "<null>"
	}
	return *v
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}
