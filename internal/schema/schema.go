// SPDX-License-Identifier: Apache-2.0

// Package schema introspects tracked tables: primary keys, column types,
// replica identity, foreign keys, and detects schema drift.
package schema

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/pg"
)

// Ref names a table.
type Ref struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

func (r Ref) String() string { return r.Schema + "." + r.Name }

// Qualified returns the quoted, schema-qualified name.
func (r Ref) Qualified() string { return pg.QualifiedName(r.Schema, r.Name) }

// Less orders refs by (schema, name).
func (r Ref) Less(o Ref) bool {
	if r.Schema != o.Schema {
		return r.Schema < o.Schema
	}
	return r.Name < o.Name
}

// ParseRefs parses "schema.name,schema.name". A name without a schema is in
// "public".
func ParseRefs(s string) ([]Ref, error) {
	var refs []Ref
	seen := map[Ref]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r := Ref{Schema: "public", Name: part}
		if i := strings.IndexByte(part, '.'); i >= 0 {
			r = Ref{Schema: part[:i], Name: part[i+1:]}
		}
		if r.Schema == "" || r.Name == "" {
			return nil, fmt.Errorf("invalid table name %q (want schema.name)", part)
		}
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no tables given")
	}
	return refs, nil
}

// Column is a writable column of a tracked table.
type Column struct {
	Name string `json:"name"`
	// Type is format_type(atttypid, atttypmod) computed with
	// search_path = pg_catalog, so user-defined types are schema-qualified.
	Type string `json:"type"`
	// Identity is "a" (GENERATED ALWAYS), "d" (BY DEFAULT) or "".
	Identity string `json:"-"`
}

// Table describes a tracked table. Columns lists non-dropped, non-generated
// columns in attnum order; generated columns are never compared or written.
type Table struct {
	Ref
	PK           []string `json:"pk"`
	Columns      []Column `json:"columns"`
	PKConstraint string   `json:"-"`
	OID          uint32   `json:"-"`
	Kind         string   `json:"-"`
	Persistence  string   `json:"-"`
	IsPartition  bool     `json:"-"`
	ReplIdent    string   `json:"-"`
	Owned        bool     `json:"-"`
}

// Column returns the column with the given name.
func (t *Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// IsPK reports whether name is a primary-key column.
func (t *Table) IsPK(name string) bool {
	for _, k := range t.PK {
		if k == name {
			return true
		}
	}
	return false
}

// HasAlwaysIdentity reports whether a column is GENERATED ALWAYS AS IDENTITY.
func (t *Table) HasAlwaysIdentity() bool {
	for _, c := range t.Columns {
		if c.Identity == "a" {
			return true
		}
	}
	return false
}

// Problems lists the reasons the table cannot be tracked (spec §1.4, §2).
func (t *Table) Problems() []string {
	var p []string
	switch {
	case t.Kind == "p":
		p = append(p, "partitioned tables are not supported")
	case t.IsPartition:
		p = append(p, "partitions are not supported")
	case t.Kind != "r":
		p = append(p, fmt.Sprintf("relation kind %q is not an ordinary table", t.Kind))
	}
	if t.Persistence != "p" {
		p = append(p, "unlogged and temporary tables are not supported")
	}
	if len(t.PK) == 0 {
		p = append(p, "table has no PRIMARY KEY")
	}
	if !t.Owned {
		p = append(p, "the connected role does not own the table")
	}
	return p
}

// WithCatalogPath runs fn in a transaction whose search_path is pg_catalog,
// so format_type returns schema-qualified names for user-defined types.
func WithCatalogPath(ctx context.Context, conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('search_path', 'pg_catalog', true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Load introspects the given tables. A missing table is an error.
func Load(ctx context.Context, conn *pgx.Conn, refs []Ref) ([]*Table, error) {
	var tables []*Table
	err := WithCatalogPath(ctx, conn, func(tx pgx.Tx) error {
		for _, r := range refs {
			t, err := loadOne(ctx, tx, r)
			if err != nil {
				return err
			}
			tables = append(tables, t)
		}
		return nil
	})
	return tables, err
}

func loadOne(ctx context.Context, q pg.Querier, r Ref) (*Table, error) {
	t := &Table{Ref: r}
	err := q.QueryRow(ctx, `
SELECT c.oid, c.relkind::text, c.relpersistence::text, c.relispartition, c.relreplident::text,
       pg_has_role(c.relowner, 'MEMBER')
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, r.Schema, r.Name).Scan(
		&t.OID, &t.Kind, &t.Persistence, &t.IsPartition, &t.ReplIdent, &t.Owned)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("table %s does not exist", r)
	}
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", r, err)
	}
	rows, err := q.Query(ctx, `
SELECT a.attname::text, format_type(a.atttypid, a.atttypmod), a.attidentity::text
FROM pg_attribute a
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
ORDER BY a.attnum`, t.OID)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", r, err)
	}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Identity); err != nil {
			return nil, err
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", r, err)
	}
	err = q.QueryRow(ctx, `
SELECT con.conname::text, array_agg(a.attname::text ORDER BY k.ord)
FROM pg_constraint con
CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
WHERE con.conrelid = $1 AND con.contype = 'p'
GROUP BY con.conname`, t.OID).Scan(&t.PKConstraint, &t.PK)
	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("read primary key of %s: %w", r, err)
	}
	return t, nil
}

// ListTables returns the ordinary, non-partition tables in the given schemas.
func ListTables(ctx context.Context, q pg.Querier, schemas []string) ([]Ref, error) {
	rows, err := q.Query(ctx, `
SELECT n.nspname::text, c.relname::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND NOT c.relispartition AND c.relpersistence = 'p'
  AND n.nspname = ANY($1)
ORDER BY 1, 2`, schemas)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ref, error) {
		var r Ref
		err := row.Scan(&r.Schema, &r.Name)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return refs, nil
}

// Drift compares other (for example the branch's definition) against main
// and returns a human-readable list of differences in column names, order,
// types and primary key. Types are compared by name, never by OID.
func Drift(main, other *Table) []string {
	diffs := CompareColumns(main, other.Columns)
	if strings.Join(main.PK, ",") != strings.Join(other.PK, ",") {
		diffs = append(diffs, fmt.Sprintf("primary key: main (%s), branch (%s)",
			strings.Join(main.PK, ", "), strings.Join(other.PK, ", ")))
	}
	return diffs
}

// CompareColumns compares an ordered list of columns against main's.
func CompareColumns(main *Table, cols []Column) []string {
	var diffs []string
	n := len(main.Columns)
	if len(cols) > n {
		n = len(cols)
	}
	for i := 0; i < n; i++ {
		switch {
		case i >= len(cols):
			diffs = append(diffs, fmt.Sprintf("column %d: main has %s %s, branch has none", i+1, main.Columns[i].Name, main.Columns[i].Type))
		case i >= len(main.Columns):
			diffs = append(diffs, fmt.Sprintf("column %d: branch has %s %s, main has none", i+1, cols[i].Name, cols[i].Type))
		case main.Columns[i].Name != cols[i].Name || main.Columns[i].Type != cols[i].Type:
			diffs = append(diffs, fmt.Sprintf("column %d: main has %s %s, branch has %s %s", i+1,
				main.Columns[i].Name, main.Columns[i].Type, cols[i].Name, cols[i].Type))
		}
	}
	return diffs
}

// FKEdge says that Child references Parent.
type FKEdge struct {
	Child, Parent Ref
}

// ForeignKeys returns the foreign keys among the given tables, sorted.
func ForeignKeys(ctx context.Context, q pg.Querier, tables []Ref) ([]FKEdge, error) {
	tracked := map[Ref]bool{}
	for _, r := range tables {
		tracked[r] = true
	}
	rows, err := q.Query(ctx, `
SELECT cn.nspname::text, c.relname::text, pn.nspname::text, p.relname::text
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace cn ON cn.oid = c.relnamespace
JOIN pg_catalog.pg_class p ON p.oid = con.confrelid
JOIN pg_catalog.pg_namespace pn ON pn.oid = p.relnamespace
WHERE con.contype = 'f'`)
	if err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	var edges []FKEdge
	seen := map[FKEdge]bool{}
	for rows.Next() {
		var e FKEdge
		if err := rows.Scan(&e.Child.Schema, &e.Child.Name, &e.Parent.Schema, &e.Parent.Name); err != nil {
			return nil, err
		}
		if tracked[e.Child] && tracked[e.Parent] && !seen[e] {
			seen[e] = true
			edges = append(edges, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Child != edges[j].Child {
			return edges[i].Child.Less(edges[j].Child)
		}
		return edges[i].Parent.Less(edges[j].Parent)
	})
	return edges, nil
}

// KeySequence returns the sequence owned by a primary-key column of t
// (serial or identity), or "" if there is none.
func KeySequence(ctx context.Context, q pg.Querier, t *Table) (seq, column string, err error) {
	for _, col := range t.PK {
		var s *string
		if err := q.QueryRow(ctx, "SELECT pg_catalog.pg_get_serial_sequence($1, $2)", t.Qualified(), col).Scan(&s); err != nil {
			return "", "", fmt.Errorf("look up sequence of %s.%s: %w", t, col, err)
		}
		if s != nil {
			return *s, col, nil
		}
	}
	return "", "", nil
}
