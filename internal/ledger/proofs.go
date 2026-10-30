// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Proof is the output of `pgweir prove` (spec §11.4).
type Proof struct {
	LeafIndex  uint64          `json:"leaf_index"`
	LeafRecord json.RawMessage `json:"leaf_record"`
	LeafHash   string          `json:"leaf_hash"`
	TreeSize   uint64          `json:"tree_size"`
	AuditPath  []string        `json:"audit_path"`
	STH        STH             `json:"sth"`
	Signature  string          `json:"signature"`
	PublicKey  string          `json:"public_key,omitempty"`
}

// VerifyProof checks a proof offline: (1) the leaf hash of the record,
// (2) the inclusion path to the tree head's root, (3) the signature over the
// tree head with pub.
func VerifyProof(p *Proof, pub ed25519.PublicKey) error {
	_, leafHash, err := LeafHash(p.LeafRecord)
	if err != nil {
		return fmt.Errorf("leaf_record is not valid JSON: %w", err)
	}
	if hex.EncodeToString(leafHash) != p.LeafHash {
		return fmt.Errorf("leaf_hash does not match the leaf record (record altered?)")
	}
	if p.TreeSize != p.STH.TreeSize {
		return fmt.Errorf("proof is for tree size %d but the tree head has size %d", p.TreeSize, p.STH.TreeSize)
	}
	if p.LeafIndex >= p.TreeSize {
		return fmt.Errorf("leaf index %d outside tree of size %d", p.LeafIndex, p.TreeSize)
	}
	path, err := unhexAll(p.AuditPath)
	if err != nil {
		return err
	}
	root, err := hex.DecodeString(p.STH.RootHash)
	if err != nil {
		return fmt.Errorf("invalid root hash: %w", err)
	}
	if err := VerifyInclusion(p.LeafIndex, p.TreeSize, leafHash, path, root); err != nil {
		return fmt.Errorf("inclusion proof does not lead to the signed root: %w", err)
	}
	sig, err := hex.DecodeString(p.Signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	return p.STH.Verify(pub, sig)
}

// ConsistencyProof is the output of `pgweir ledger consistency`.
type ConsistencyProof struct {
	From  SignedSTH `json:"from"`
	To    SignedSTH `json:"to"`
	Proof []string  `json:"proof"`
}

// Verify checks the consistency proof and, if pub is given, both signatures.
func (c *ConsistencyProof) Verify(pub ed25519.PublicKey) error {
	if pub != nil {
		for _, s := range []SignedSTH{c.From, c.To} {
			if err := s.Verify(pub); err != nil {
				return err
			}
		}
	}
	path, err := unhexAll(c.Proof)
	if err != nil {
		return err
	}
	r1, err := hex.DecodeString(c.From.STH.RootHash)
	if err != nil {
		return err
	}
	r2, err := hex.DecodeString(c.To.STH.RootHash)
	if err != nil {
		return err
	}
	if err := VerifyConsistency(c.From.STH.TreeSize, c.To.STH.TreeSize, path, r1, r2); err != nil {
		return fmt.Errorf("tree %d is not an append-only extension of tree %d: %w", c.To.STH.TreeSize, c.From.STH.TreeSize, err)
	}
	return nil
}

// Verify checks the signature of a signed tree head.
func (s SignedSTH) Verify(pub ed25519.PublicKey) error {
	sig, err := hex.DecodeString(s.Signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	return s.STH.Verify(pub, sig)
}

// ConsistencySource returns a consistency proof between two tree sizes of
// the current ledger, and the current tree head.
type ConsistencySource interface {
	Consistency(size1, size2 uint64) ([][]byte, error)
	Root(size uint64) ([]byte, error)
	Size() uint64
}

// VerifyChain checks that a set of previously published tree heads is
// signed by one of pubs (several after a key rotation) and that each is an
// append-only prefix of the next and of the current ledger (spec §11.4
// verify-chain).
func VerifyChain(sths []SignedSTH, pubs []ed25519.PublicKey, current ConsistencySource) error {
	if len(sths) == 0 {
		return fmt.Errorf("no tree heads given")
	}
	keys := map[string]ed25519.PublicKey{}
	for _, p := range pubs {
		keys[KeyID(p)] = p
	}
	sorted := make([]SignedSTH, len(sths))
	copy(sorted, sths)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].STH.TreeSize < sorted[j].STH.TreeSize })
	for _, s := range sorted {
		pub, ok := keys[s.STH.KeyID]
		if !ok {
			return fmt.Errorf("tree head %d was signed by key %s, which is not among the given public keys", s.STH.TreeSize, s.STH.KeyID)
		}
		if err := s.Verify(pub); err != nil {
			return err
		}
	}
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1].STH, sorted[i].STH
		if a.TreeSize == b.TreeSize && a.RootHash != b.RootHash {
			return fmt.Errorf("two signed tree heads of size %d have different roots (fork)", a.TreeSize)
		}
	}
	// Check every published head against the current ledger: the root of the
	// current tree at that size must match, and the published sequence must
	// be consistent. Comparing roots at each size against the current tree
	// and proving consistency to the current head covers both.
	head := current.Size()
	headRoot, err := current.Root(head)
	if err != nil {
		return err
	}
	for i, s := range sorted {
		if s.STH.TreeSize > head {
			return fmt.Errorf("tree head %d is larger than the current ledger (%d leaves): leaves were removed", s.STH.TreeSize, head)
		}
		path, err := current.Consistency(s.STH.TreeSize, head)
		if err != nil {
			return err
		}
		old, err := hex.DecodeString(s.STH.RootHash)
		if err != nil {
			return err
		}
		if err := VerifyConsistency(s.STH.TreeSize, head, path, old, headRoot); err != nil {
			return fmt.Errorf("published tree head %d (#%d) is not a prefix of the current ledger: %w", s.STH.TreeSize, i+1, err)
		}
	}
	return nil
}
