// SPDX-License-Identifier: Apache-2.0

// Package ledger records every applied merge as a leaf of an RFC 6962 Merkle
// tree and signs each tree head with ed25519 (spec §11).
//
// The ledger is tamper-evident, not tamper-proof: anyone with write access to
// main can rewrite it. Rewrites are detected by checking proofs and tree heads
// exported earlier (or anchored elsewhere) against the current ledger.
package ledger

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"

	"github.com/keeptalking83/pgweir/internal/changeset"
)

var hasher = rfc6962.DefaultHasher

// TimeFormat is RFC 3339 in UTC with milliseconds.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// FormatTime formats t for records and tree heads.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Record is the canonical merge record stored in a leaf (spec §11.1).
type Record struct {
	V             int               `json:"v"`
	MergeID       string            `json:"merge_id"`
	BranchID      string            `json:"branch_id"`
	BranchName    string            `json:"branch_name"`
	BaseLSN       string            `json:"base_lsn"`
	EndLSN        string            `json:"end_lsn"`
	MainLSNAfter  string            `json:"main_lsn_after"`
	ChangesetHash string            `json:"changeset_hash"`
	Policy        string            `json:"policy"`
	Stats         json.RawMessage   `json:"stats"`
	ConflictsHash string            `json:"conflicts_hash"`
	Runs          []json.RawMessage `json:"runs"`
	AppliedAt     string            `json:"applied_at"`
}

// LeafHash returns the canonical bytes of a record and their RFC 6962 leaf
// hash, SHA256(0x00 || jcs(record)).
func LeafHash(record any) (canonical, leafHash []byte, err error) {
	canonical, err = changeset.Canonical(record)
	if err != nil {
		return nil, nil, err
	}
	return canonical, hasher.HashLeaf(canonical), nil
}

// STH is a tree head (spec §11.2).
type STH struct {
	V         int    `json:"v"`
	TreeSize  uint64 `json:"tree_size"`
	RootHash  string `json:"root_hash"`
	Timestamp string `json:"timestamp"`
	KeyID     string `json:"key_id"`
}

// SignedSTH is a tree head with its signature, as printed by `ledger sth`
// and exported to sth/<tree_size>.json.
type SignedSTH struct {
	STH       STH    `json:"sth"`
	Signature string `json:"signature"`
	PublicKey string `json:"public_key,omitempty"`
}

// Sign signs the canonical JSON of the tree head.
func (s STH) Sign(key ed25519.PrivateKey) ([]byte, error) {
	msg, err := changeset.Canonical(s)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(key, msg), nil
}

// Verify checks sig over the canonical JSON of the tree head with pub, and
// that the tree head names pub's key id.
func (s STH) Verify(pub ed25519.PublicKey, sig []byte) error {
	if s.KeyID != KeyID(pub) {
		return fmt.Errorf("tree head was signed by key %s, not by the given public key %s", s.KeyID, KeyID(pub))
	}
	msg, err := changeset.Canonical(s)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("signature over tree head %d does not verify", s.TreeSize)
	}
	return nil
}

// KeyID is the first 16 hex characters of sha256(public key).
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])[:16]
}

// Tree is an in-memory view of the ledger's leaf hashes, enough to compute
// roots and proofs. Ledgers grow by one leaf per merge, so loading every leaf
// hash (32 bytes each) is cheap.
type Tree struct {
	leaves [][]byte
}

// NewTree returns a tree over the given leaf hashes.
func NewTree(leafHashes [][]byte) *Tree { return &Tree{leaves: leafHashes} }

// Size returns the number of leaves.
func (t *Tree) Size() uint64 { return uint64(len(t.leaves)) }

// Root returns the root hash of the first size leaves.
func (t *Tree) Root(size uint64) ([]byte, error) {
	if size > t.Size() {
		return nil, fmt.Errorf("tree size %d exceeds %d leaves", size, t.Size())
	}
	if size == 0 {
		return hasher.EmptyRoot(), nil
	}
	r := rangeFactory.NewEmptyRange(0)
	for _, h := range t.leaves[:size] {
		if err := r.Append(h, nil); err != nil {
			return nil, err
		}
	}
	return r.GetRootHash(nil)
}

func (t *Tree) node(id compact.NodeID) []byte {
	if id.Level == 0 {
		return t.leaves[id.Index]
	}
	l := t.node(compact.NewNodeID(id.Level-1, id.Index*2))
	r := t.node(compact.NewNodeID(id.Level-1, id.Index*2+1))
	return hasher.HashChildren(l, r)
}

func (t *Tree) materialize(n proof.Nodes) ([][]byte, error) {
	hashes := make([][]byte, len(n.IDs))
	for i, id := range n.IDs {
		hashes[i] = t.node(id)
	}
	return n.Rehash(hashes, hasher.HashChildren)
}

// Inclusion returns the audit path of leaf index in the tree of size.
func (t *Tree) Inclusion(index, size uint64) ([][]byte, error) {
	if size > t.Size() {
		return nil, fmt.Errorf("tree size %d exceeds %d leaves", size, t.Size())
	}
	n, err := proof.Inclusion(index, size)
	if err != nil {
		return nil, err
	}
	return t.materialize(n)
}

// Consistency returns the consistency proof between two tree sizes.
func (t *Tree) Consistency(size1, size2 uint64) ([][]byte, error) {
	if size2 > t.Size() {
		return nil, fmt.Errorf("tree size %d exceeds %d leaves", size2, t.Size())
	}
	n, err := proof.Consistency(size1, size2)
	if err != nil {
		return nil, err
	}
	return t.materialize(n)
}

var rangeFactory = &compact.RangeFactory{Hash: hasher.HashChildren}

// VerifyInclusion checks an audit path against a root.
func VerifyInclusion(index, size uint64, leafHash []byte, path [][]byte, root []byte) error {
	return proof.VerifyInclusion(hasher, index, size, leafHash, path, root)
}

// VerifyConsistency checks a consistency proof between two roots.
func VerifyConsistency(size1, size2 uint64, path [][]byte, root1, root2 []byte) error {
	return proof.VerifyConsistency(hasher, size1, size2, path, root1, root2)
}

func hexAll(hs [][]byte) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = hex.EncodeToString(h)
	}
	return out
}

func unhexAll(ss []string) ([][]byte, error) {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("invalid hex %q: %w", s, err)
		}
		out[i] = b
	}
	return out, nil
}

// rangeT is a compact range over leaves [0, n).
type rangeT = *compact.Range
