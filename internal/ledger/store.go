// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/pg"
)

// Appended is the result of appending a merge record.
type Appended struct {
	LeafIndex uint64
	Record    Record
	Head      SignedSTH
}

// Signer appends merge records to the ledger inside the apply transaction.
type Signer struct {
	Key ed25519.PrivateKey
	// Now returns the current time; tests may override it.
	Now func() time.Time
}

type compactState struct {
	Size   uint64   `json:"size"`
	Hashes []string `json:"hashes"`
}

// Append adds rec as the next leaf and inserts a signed tree head for the
// new size, in tx (spec §10 step 8.7, §11.2). Appends are serialized by a
// transaction-scoped advisory lock; applied_at and the tree head timestamp
// are taken after the lock so they increase with the tree size.
func (s *Signer) Append(ctx context.Context, tx pgx.Tx, rec Record) (*Appended, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('pgweir.ledger', 0))"); err != nil {
		return nil, fmt.Errorf("lock ledger: %w", err)
	}
	size, last, err := latestState(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ts := now().UTC().Truncate(time.Millisecond)
	if last != nil {
		prev, err := time.Parse(TimeFormat, last.Timestamp)
		if err == nil && !ts.After(prev) {
			ts = prev.Add(time.Millisecond)
		}
	}
	rec.V = 1
	rec.AppliedAt = FormatTime(ts)
	canon, leafHash, err := LeafHash(rec)
	if err != nil {
		return nil, err
	}

	rng, err := loadRange(ctx, tx, size, last)
	if err != nil {
		return nil, err
	}
	if err := rng.Append(leafHash, nil); err != nil {
		return nil, err
	}
	root, err := rng.GetRootHash(nil)
	if err != nil {
		return nil, err
	}

	pub := s.Key.Public().(ed25519.PublicKey)
	sth := STH{V: 1, TreeSize: size + 1, RootHash: hex.EncodeToString(root), Timestamp: FormatTime(ts), KeyID: KeyID(pub)}
	sig, err := sth.Sign(s.Key)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO pgweir.ledger_leaves (leaf_index, leaf_hash, record) VALUES ($1, $2, $3::jsonb)",
		int64(size), leafHash, string(canon)); err != nil {
		return nil, fmt.Errorf("insert ledger leaf: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO pgweir.ledger_sth (tree_size, root_hash, timestamp, key_id, signature) VALUES ($1, $2, $3::timestamptz, $4, $5)",
		int64(sth.TreeSize), root, sth.Timestamp, sth.KeyID, sig); err != nil {
		return nil, fmt.Errorf("insert tree head: %w", err)
	}
	state := compactState{Size: rng.End(), Hashes: hexAll(rng.Hashes())}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO pgweir.config (key, value) VALUES ('ledger_compact_range', $1::jsonb)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, string(stateJSON))
	if err != nil {
		return nil, fmt.Errorf("save compact range: %w", err)
	}
	_, err = tx.Exec(ctx, `
INSERT INTO pgweir.config (key, value) VALUES ('ledger_public_keys', jsonb_build_object($1::text, $2::text))
ON CONFLICT (key) DO UPDATE SET value = pgweir.config.value || EXCLUDED.value`, sth.KeyID, hex.EncodeToString(pub))
	if err != nil {
		return nil, fmt.Errorf("record public key: %w", err)
	}
	return &Appended{LeafIndex: size, Record: rec, Head: SignedSTH{STH: sth, Signature: hex.EncodeToString(sig), PublicKey: hex.EncodeToString(pub)}}, nil
}

// latestState returns the number of leaves and the latest tree head.
func latestState(ctx context.Context, q pg.Querier) (uint64, *STH, error) {
	var n int64
	if err := q.QueryRow(ctx, "SELECT coalesce(max(leaf_index) + 1, 0) FROM pgweir.ledger_leaves").Scan(&n); err != nil {
		return 0, nil, fmt.Errorf("read ledger size: %w", err)
	}
	if n == 0 {
		return 0, nil, nil
	}
	head, err := LoadSTH(ctx, q, 0)
	if err != nil {
		return 0, nil, err
	}
	if head.STH.TreeSize != uint64(n) {
		return 0, nil, fmt.Errorf("ledger has %d leaves but its latest signed tree head has size %d: the ledger was altered", n, head.STH.TreeSize)
	}
	return uint64(n), &head.STH, nil
}

// loadRange returns the compact range over the existing leaves. A stored
// range that is missing, of the wrong size or inconsistent with the latest
// signed root is rebuilt from the leaf hashes; if those do not match the
// signed root either, the ledger was altered and nothing is appended.
func loadRange(ctx context.Context, q pg.Querier, size uint64, last *STH) (rangeT, error) {
	var signedRoot []byte
	if last != nil {
		var err error
		if signedRoot, err = hex.DecodeString(last.RootHash); err != nil {
			return nil, err
		}
	}
	matches := func(r rangeT) bool {
		if size == 0 {
			return true
		}
		root, err := r.GetRootHash(nil)
		return err == nil && bytes.Equal(root, signedRoot)
	}
	var raw []byte
	err := q.QueryRow(ctx, "SELECT value FROM pgweir.config WHERE key = 'ledger_compact_range'").Scan(&raw)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	if raw != nil {
		var st compactState
		if json.Unmarshal(raw, &st) == nil && st.Size == size {
			if hashes, err := unhexAll(st.Hashes); err == nil {
				if r, err := rangeFactory.NewRange(0, size, hashes); err == nil && matches(r) {
					return r, nil
				}
			}
		}
	}
	tree, err := LoadTree(ctx, q, size)
	if err != nil {
		return nil, err
	}
	r := rangeFactory.NewEmptyRange(0)
	for _, h := range tree.leaves {
		if err := r.Append(h, nil); err != nil {
			return nil, err
		}
	}
	if !matches(r) {
		return nil, fmt.Errorf("ledger leaves do not match the latest signed tree head (size %d): the ledger was altered; refusing to extend it", size)
	}
	return r, nil
}

// LoadTree reads the first size leaf hashes (all leaves if size is 0).
func LoadTree(ctx context.Context, q pg.Querier, size uint64) (*Tree, error) {
	sql := "SELECT leaf_index, leaf_hash FROM pgweir.ledger_leaves ORDER BY leaf_index"
	args := []any{}
	if size > 0 {
		sql = "SELECT leaf_index, leaf_hash FROM pgweir.ledger_leaves WHERE leaf_index < $1 ORDER BY leaf_index"
		args = append(args, int64(size))
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read ledger leaves: %w", err)
	}
	defer rows.Close()
	var leaves [][]byte
	for rows.Next() {
		var idx int64
		var h []byte
		if err := rows.Scan(&idx, &h); err != nil {
			return nil, err
		}
		if idx != int64(len(leaves)) {
			return nil, fmt.Errorf("ledger leaf %d is missing: the ledger was altered", len(leaves))
		}
		leaves = append(leaves, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if size > 0 && uint64(len(leaves)) != size {
		return nil, fmt.Errorf("ledger has %d leaves, expected %d", len(leaves), size)
	}
	return NewTree(leaves), nil
}

// LoadSTH reads the signed tree head of the given size (0: the latest).
func LoadSTH(ctx context.Context, q pg.Querier, size uint64) (*SignedSTH, error) {
	sql := "SELECT tree_size, root_hash, timestamp, key_id, signature FROM pgweir.ledger_sth "
	args := []any{}
	if size == 0 {
		sql += "ORDER BY tree_size DESC LIMIT 1"
	} else {
		sql += "WHERE tree_size = $1"
		args = append(args, int64(size))
	}
	var n int64
	var root, sig []byte
	var ts time.Time
	var keyID string
	err := q.QueryRow(ctx, sql, args...).Scan(&n, &root, &ts, &keyID, &sig)
	if err == pgx.ErrNoRows {
		if size == 0 {
			return nil, errs.New(errs.InvalidArgument, "the ledger is empty: no merge has been applied yet")
		}
		return nil, errs.New(errs.InvalidArgument, "no signed tree head of size %d", size)
	}
	if err != nil {
		return nil, fmt.Errorf("read tree head: %w", err)
	}
	s := &SignedSTH{
		STH:       STH{V: 1, TreeSize: uint64(n), RootHash: hex.EncodeToString(root), Timestamp: FormatTime(ts), KeyID: keyID},
		Signature: hex.EncodeToString(sig),
	}
	s.PublicKey, _ = PublicKeyFor(ctx, q, keyID)
	return s, nil
}

// ListSTHs returns every signed tree head in tree size order.
func ListSTHs(ctx context.Context, q pg.Querier) ([]SignedSTH, error) {
	rows, err := q.Query(ctx, "SELECT tree_size, root_hash, timestamp, key_id, signature FROM pgweir.ledger_sth ORDER BY tree_size")
	if err != nil {
		return nil, fmt.Errorf("read tree heads: %w", err)
	}
	var out []SignedSTH
	for rows.Next() {
		var n int64
		var root, sig []byte
		var ts time.Time
		var keyID string
		if err := rows.Scan(&n, &root, &ts, &keyID, &sig); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, SignedSTH{
			STH:       STH{V: 1, TreeSize: uint64(n), RootHash: hex.EncodeToString(root), Timestamp: FormatTime(ts), KeyID: keyID},
			Signature: hex.EncodeToString(sig),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].PublicKey, _ = PublicKeyFor(ctx, q, out[i].STH.KeyID)
	}
	return out, nil
}

// PublicKeyFor returns the hex public key recorded for a key id.
func PublicKeyFor(ctx context.Context, q pg.Querier, keyID string) (string, error) {
	var pub *string
	err := q.QueryRow(ctx, "SELECT value ->> $1 FROM pgweir.config WHERE key = 'ledger_public_keys'", keyID).Scan(&pub)
	if err != nil || pub == nil {
		return "", err
	}
	return *pub, nil
}

// Prove builds the inclusion proof of a merge against the latest tree head.
func Prove(ctx context.Context, q pg.Querier, mergeID string) (*Proof, error) {
	var leafIndex *int64
	var status string
	err := q.QueryRow(ctx, "SELECT leaf_index, status FROM pgweir.merges WHERE id = $1::uuid", mergeID).Scan(&leafIndex, &status)
	if err == pgx.ErrNoRows {
		return nil, errs.New(errs.InvalidArgument, "merge %s does not exist", mergeID)
	}
	if err != nil {
		return nil, fmt.Errorf("read merge %s: %w", mergeID, err)
	}
	if leafIndex == nil {
		return nil, errs.New(errs.InvalidArgument, "merge %s is %s and has no ledger leaf (only applied merges are recorded)", mergeID, status)
	}
	head, err := LoadSTH(ctx, q, 0)
	if err != nil {
		return nil, err
	}
	tree, err := LoadTree(ctx, q, head.STH.TreeSize)
	if err != nil {
		return nil, err
	}
	idx := uint64(*leafIndex)
	path, err := tree.Inclusion(idx, head.STH.TreeSize)
	if err != nil {
		return nil, err
	}
	var record []byte
	if err := q.QueryRow(ctx, "SELECT record FROM pgweir.ledger_leaves WHERE leaf_index = $1", *leafIndex).Scan(&record); err != nil {
		return nil, fmt.Errorf("read ledger leaf %d: %w", idx, err)
	}
	canon, _, err := LeafHash(json.RawMessage(record))
	if err != nil {
		return nil, err
	}
	return &Proof{
		LeafIndex: idx, LeafRecord: canon, LeafHash: hex.EncodeToString(tree.leaves[idx]),
		TreeSize: head.STH.TreeSize, AuditPath: hexAll(path),
		STH: head.STH, Signature: head.Signature, PublicKey: head.PublicKey,
	}, nil
}

// Consistency builds a consistency proof between two signed tree heads.
func Consistency(ctx context.Context, q pg.Querier, from, to uint64) (*ConsistencyProof, error) {
	if from == 0 || from > to {
		return nil, errs.New(errs.InvalidArgument, "need 1 <= --from <= --to")
	}
	a, err := LoadSTH(ctx, q, from)
	if err != nil {
		return nil, err
	}
	b, err := LoadSTH(ctx, q, to)
	if err != nil {
		return nil, err
	}
	tree, err := LoadTree(ctx, q, to)
	if err != nil {
		return nil, err
	}
	path, err := tree.Consistency(from, to)
	if err != nil {
		return nil, err
	}
	return &ConsistencyProof{From: *a, To: *b, Proof: hexAll(path)}, nil
}
