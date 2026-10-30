// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/pg"
)

// ErrExists is returned by Sink.Put when an object exists with other
// content: exported objects are never overwritten.
var ErrExists = errors.New("object exists with different content")

// Sink is an append-only object store (a directory or an S3 prefix).
type Sink interface {
	// List returns the object names under prefix, relative to the sink root.
	List(ctx context.Context, prefix string) ([]string, error)
	// Put writes an object unless it exists. It reports whether it wrote;
	// an existing object with different content is ErrExists.
	Put(ctx context.Context, name string, data []byte) (bool, error)
	String() string
}

// ExportOptions configure Export.
type ExportOptions struct {
	Segment           int
	IncludeChangesets bool
}

// ExportStats counts written objects.
type ExportStats struct {
	LeafFiles, STHFiles, Changesets int
	Leaves                          uint64
}

type exportedLeaf struct {
	LeafIndex uint64          `json:"leaf_index"`
	LeafHash  string          `json:"leaf_hash"`
	Record    json.RawMessage `json:"record"`
}

// Export writes leaves not yet exported (leaves/<first>-<last>.json, at most
// Segment leaves per file, files aligned to Segment boundaries), every signed
// tree head (sth/<tree_size>.json) and, optionally, the canonical changeset
// of every applied merge (changesets/<merge_id>.json). Existing objects are
// never overwritten (spec §11.5).
func Export(ctx context.Context, q pg.Querier, sink Sink, opts ExportOptions) (ExportStats, error) {
	var st ExportStats
	if opts.Segment <= 0 {
		opts.Segment = 1000
	}
	head, err := LoadSTH(ctx, q, 0)
	if err != nil {
		return st, err
	}
	next, err := nextUnexported(ctx, sink)
	if err != nil {
		return st, err
	}
	seg := uint64(opts.Segment)
	for start := next; start < head.STH.TreeSize; {
		end := min((start/seg+1)*seg, head.STH.TreeSize) // exclusive
		leaves, err := loadLeaves(ctx, q, start, end)
		if err != nil {
			return st, err
		}
		data, err := changeset.Canonical(leaves)
		if err != nil {
			return st, err
		}
		name := fmt.Sprintf("leaves/%08d-%08d.json", start, end-1)
		if _, err := sink.Put(ctx, name, data); err != nil {
			return st, fmt.Errorf("export %s: %w", name, err)
		}
		st.LeafFiles++
		st.Leaves += end - start
		start = end
	}

	heads, err := ListSTHs(ctx, q)
	if err != nil {
		return st, err
	}
	for _, h := range heads {
		data, err := changeset.Canonical(h)
		if err != nil {
			return st, err
		}
		name := fmt.Sprintf("sth/%d.json", h.STH.TreeSize)
		wrote, err := sink.Put(ctx, name, data)
		if err != nil {
			return st, fmt.Errorf("export %s: %w", name, err)
		}
		if wrote {
			st.STHFiles++
		}
	}

	if opts.IncludeChangesets {
		n, err := exportChangesets(ctx, q, sink)
		st.Changesets = n
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

func nextUnexported(ctx context.Context, sink Sink) (uint64, error) {
	names, err := sink.List(ctx, "leaves/")
	if err != nil {
		return 0, err
	}
	var next uint64
	for _, n := range names {
		base := strings.TrimSuffix(path.Base(n), ".json")
		parts := strings.Split(base, "-")
		if len(parts) != 2 {
			continue
		}
		last, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if last+1 > next {
			next = last + 1
		}
	}
	return next, nil
}

func loadLeaves(ctx context.Context, q pg.Querier, start, end uint64) ([]exportedLeaf, error) {
	rows, err := q.Query(ctx, "SELECT leaf_index, leaf_hash, record FROM pgweir.ledger_leaves WHERE leaf_index >= $1 AND leaf_index < $2 ORDER BY leaf_index",
		int64(start), int64(end))
	if err != nil {
		return nil, fmt.Errorf("read ledger leaves: %w", err)
	}
	defer rows.Close()
	var out []exportedLeaf
	for rows.Next() {
		var idx int64
		var h, rec []byte
		if err := rows.Scan(&idx, &h, &rec); err != nil {
			return nil, err
		}
		canon, _, err := LeafHash(json.RawMessage(rec))
		if err != nil {
			return nil, err
		}
		out = append(out, exportedLeaf{LeafIndex: uint64(idx), LeafHash: hex.EncodeToString(h), Record: canon})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if uint64(len(out)) != end-start {
		return nil, fmt.Errorf("ledger leaves %d..%d are incomplete: the ledger was altered", start, end-1)
	}
	return out, nil
}

// exportChangesets rebuilds the canonical changeset of every applied merge
// from pgweir.events and checks it against the recorded hash.
func exportChangesets(ctx context.Context, q pg.Querier, sink Sink) (int, error) {
	rows, err := q.Query(ctx, `
SELECT m.id::text, b.id::text, b.name, b.base_lsn::text, m.end_lsn::text, m.changeset_tables, m.changeset_hash
FROM pgweir.merges m JOIN pgweir.branches b ON b.id = m.branch_id
WHERE m.status = 'APPLIED' ORDER BY m.leaf_index`)
	if err != nil {
		return 0, fmt.Errorf("read merges: %w", err)
	}
	type mergeRow struct {
		id     string
		meta   changeset.Meta
		tables []byte
		hash   []byte
	}
	var merges []mergeRow
	for rows.Next() {
		var m mergeRow
		if err := rows.Scan(&m.id, &m.meta.BranchID, &m.meta.BranchName, &m.meta.BaseLSN, &m.meta.EndLSN, &m.tables, &m.hash); err != nil {
			rows.Close()
			return 0, err
		}
		merges = append(merges, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	written := 0
	for _, m := range merges {
		var tables []changeset.TableDef
		if err := json.Unmarshal(m.tables, &tables); err != nil {
			return written, fmt.Errorf("merge %s: decode tables: %w", m.id, err)
		}
		events, err := changeset.LoadEvents(ctx, q, m.meta.BranchID)
		if err != nil {
			return written, err
		}
		end, err := pglogrepl.ParseLSN(m.meta.EndLSN)
		if err != nil {
			return written, err
		}
		var covered []changeset.Event
		for _, e := range events {
			lsn, err := pglogrepl.ParseLSN(e.CommitLSN)
			if err != nil {
				return written, err
			}
			if lsn <= end {
				covered = append(covered, e)
			}
		}
		cs, err := changeset.Build(m.meta, tables, covered)
		if err != nil {
			return written, err
		}
		data, err := cs.Canonical()
		if err != nil {
			return written, err
		}
		h, _ := cs.Hash()
		if !bytes.Equal(h, m.hash) {
			return written, fmt.Errorf("merge %s: the changeset rebuilt from pgweir.events does not match the recorded changeset_hash: events were altered", m.id)
		}
		wrote, err := sink.Put(ctx, "changesets/"+m.id+".json", data)
		if err != nil {
			return written, err
		}
		if wrote {
			written++
		}
	}
	return written, nil
}

// DirSink exports to a local directory.
type DirSink struct {
	Root string
}

func (d DirSink) String() string { return d.Root }

func (d DirSink) List(_ context.Context, prefix string) ([]string, error) {
	dir := filepath.Join(d.Root, filepath.FromSlash(prefix))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, path.Join(prefix, e.Name()))
		}
	}
	sort.Strings(names)
	return names, nil
}

func (d DirSink) Put(_ context.Context, name string, data []byte) (bool, error) {
	p := filepath.Join(d.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return false, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G302 G304 -- exported ledger objects are public
	if errors.Is(err, fs.ErrExist) {
		existing, rerr := os.ReadFile(p) // #nosec G304 -- inside the export root
		if rerr != nil {
			return false, rerr
		}
		if !bytes.Equal(existing, data) {
			return false, fmt.Errorf("%s: %w", p, ErrExists)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}
