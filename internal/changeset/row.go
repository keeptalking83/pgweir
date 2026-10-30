// SPDX-License-Identifier: Apache-2.0

// Package changeset turns captured row events into the collapsed,
// canonical changeset of a branch (spec §8) and hashes it.
package changeset

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gowebpki/jcs"
)

// Row is a row in pgweir's JSON encoding: column name to the text
// representation of the value, nil for NULL (spec §7, "Row JSON encoding").
type Row map[string]*string

// Equal reports per-column text/NULL equality. Two nil rows are equal; a nil
// row never equals a non-nil one.
func (r Row) Equal(o Row) bool {
	if (r == nil) != (o == nil) {
		return false
	}
	if len(r) != len(o) {
		return false
	}
	for k, v := range r {
		w, ok := o[k]
		if !ok || !ValueEqual(v, w) {
			return false
		}
	}
	return true
}

// ValueEqual compares two column values; NULL equals only NULL.
func ValueEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Clone returns a copy of the row (values are shared, they are immutable).
func (r Row) Clone() Row {
	if r == nil {
		return nil
	}
	c := make(Row, len(r))
	for k, v := range r {
		c[k] = v
	}
	return c
}

// Project returns the subset of r with the given columns.
func (r Row) Project(cols []string) Row {
	p := make(Row, len(cols))
	for _, c := range cols {
		p[c] = r[c]
	}
	return p
}

// Columns returns the column names in sorted order.
func (r Row) Columns() []string {
	cols := make([]string, 0, len(r))
	for k := range r {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	return cols
}

// Canonical returns the RFC 8785 canonical JSON of v.
func Canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, err := jcs.Transform(b)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return out, nil
}

// Key returns the canonical JSON of a primary key, used as a map key and
// sort key.
func Key(pk Row) string {
	b, err := Canonical(pk)
	if err != nil {
		// A Row always marshals; canonicalization of valid JSON cannot fail.
		panic(err)
	}
	return string(b)
}

// Str returns a pointer to s, for building rows in tests and code.
func Str(s string) *string { return &s }
