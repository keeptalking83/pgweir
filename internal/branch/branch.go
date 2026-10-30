// SPDX-License-Identifier: Apache-2.0

// Package branch manages the branch registry (pgweir.branches) and the
// branch lifecycle: create, list, drop.
package branch

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	schemasql "github.com/keeptalking83/pgweir/sql"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// Branch statuses.
const (
	StatusOpen    = "OPEN"
	StatusMerged  = "MERGED"
	StatusDropped = "DROPPED"
	StatusFailed  = "FAILED"
)

// Branch is a row of pgweir.branches.
type Branch struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	ProviderRef map[string]any `json:"provider_ref"`
	DSN         string         `json:"-"`
	SlotName    string         `json:"slot_name"`
	BaseLSN     string         `json:"base_lsn"`
	Status      string         `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// ValidateName checks a branch name. Names become part of the replication
// slot name and of the local provider's database name, so they are limited
// to lowercase letters, digits and underscores.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return errs.New(errs.InvalidArgument,
			"invalid branch name %q: use 1-48 characters, lowercase letters, digits and underscores, starting with a letter", name)
	}
	return nil
}

// NewID returns a random (version 4) UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TrackedTable is an entry of the tracked_tables config key.
type TrackedTable struct {
	schema.Ref
	PK []string `json:"pk"`
}

// Config is the content of pgweir.config.
type Config struct {
	Tracked     []TrackedTable
	Publication string
}

// Refs returns the tracked table names.
func (c *Config) Refs() []schema.Ref {
	refs := make([]schema.Ref, len(c.Tracked))
	for i, t := range c.Tracked {
		refs[i] = t.Ref
	}
	return refs
}

// Initialized reports whether pgweir init has run on the database.
func Initialized(ctx context.Context, q pg.Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, "SELECT pg_catalog.to_regclass('pgweir.config') IS NOT NULL").Scan(&ok)
	return ok, err
}

// LoadConfig reads pgweir.config from main.
func LoadConfig(ctx context.Context, q pg.Querier) (*Config, error) {
	ok, err := Initialized(ctx, q)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.New(errs.NotInitialized, "pgweir is not initialized on this database: run `%s init` first", meta.Name)
	}
	var tracked []byte
	var pub string
	err = q.QueryRow(ctx, `
SELECT (SELECT value FROM pgweir.config WHERE key = 'tracked_tables'),
       coalesce((SELECT value #>> '{}' FROM pgweir.config WHERE key = 'publication'), '')`).Scan(&tracked, &pub)
	if err != nil {
		return nil, fmt.Errorf("read pgweir.config: %w", err)
	}
	if tracked == nil {
		return nil, errs.New(errs.NotInitialized, "pgweir.config has no tracked tables: run `%s init`", meta.Name)
	}
	cfg := &Config{Publication: pub}
	if err := json.Unmarshal(tracked, &cfg.Tracked); err != nil {
		return nil, fmt.Errorf("decode tracked_tables: %w", err)
	}
	return cfg, nil
}

// Install runs the schema SQL and records the tracked tables. It refuses to
// change the tracked tables while branches are open.
func Install(ctx context.Context, conn *pgx.Conn, tables []*schema.Table) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, schemasql.Schema); err != nil {
		return fmt.Errorf("install %s schema: %w", meta.Schema, err)
	}
	tracked := make([]TrackedTable, len(tables))
	for i, t := range tables {
		tracked[i] = TrackedTable{Ref: t.Ref, PK: t.PK}
	}
	newVal, err := json.Marshal(tracked)
	if err != nil {
		return err
	}
	var open int
	var same bool
	err = tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM pgweir.branches WHERE status = 'OPEN')::int,
       coalesce((SELECT value = $1::jsonb FROM pgweir.config WHERE key = 'tracked_tables'), true)`, string(newVal)).Scan(&open, &same)
	if err != nil {
		return err
	}
	if open > 0 && !same {
		return errs.New(errs.BranchState, "cannot change the tracked tables while %d branch(es) are open: merge or drop them first", open)
	}
	_, err = tx.Exec(ctx, `
INSERT INTO pgweir.config (key, value) VALUES
  ('tracked_tables', $1::jsonb),
  ('publication', to_jsonb($2::text)),
  ('schema_version', to_jsonb($3::int))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, string(newVal), meta.Publication, meta.SchemaVersion)
	if err != nil {
		return fmt.Errorf("write pgweir.config: %w", err)
	}
	return tx.Commit(ctx)
}

const selectBranch = `
SELECT id::text, name, provider, provider_ref, dsn, slot_name, base_lsn::text, status, created_at, updated_at
FROM pgweir.branches`

func scanBranch(row pgx.Row) (*Branch, error) {
	var b Branch
	var ref []byte
	if err := row.Scan(&b.ID, &b.Name, &b.Provider, &ref, &b.DSN, &b.SlotName, &b.BaseLSN, &b.Status, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(ref, &b.ProviderRef); err != nil {
		return nil, fmt.Errorf("decode provider_ref: %w", err)
	}
	return &b, nil
}

// Get returns the branch with the given name.
func Get(ctx context.Context, q pg.Querier, name string) (*Branch, error) {
	b, err := scanBranch(q.QueryRow(ctx, selectBranch+" WHERE name = $1", name))
	if err == pgx.ErrNoRows {
		return nil, errs.New(errs.InvalidArgument, "branch %q does not exist", name)
	}
	if err != nil {
		return nil, fmt.Errorf("read branch %q: %w", name, err)
	}
	return b, nil
}

// GetByID returns the branch with the given id.
func GetByID(ctx context.Context, q pg.Querier, id string) (*Branch, error) {
	b, err := scanBranch(q.QueryRow(ctx, selectBranch+" WHERE id = $1::uuid", id))
	if err != nil {
		return nil, fmt.Errorf("read branch %s: %w", id, err)
	}
	return b, nil
}

// List returns all branches, oldest first.
func List(ctx context.Context, q pg.Querier) ([]*Branch, error) {
	rows, err := q.Query(ctx, selectBranch+" ORDER BY created_at, name")
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()
	var out []*Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetStatus updates the status of a branch.
func SetStatus(ctx context.Context, q pg.Querier, id, status string) error {
	_, err := q.Exec(ctx, "UPDATE pgweir.branches SET status = $2, updated_at = now() WHERE id = $1::uuid", id, status)
	return err
}

// EventCount returns the number of captured events of a branch.
func EventCount(ctx context.Context, q pg.Querier, id string) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, "SELECT count(*) FROM pgweir.events WHERE branch_id = $1::uuid", id).Scan(&n)
	return n, err
}
