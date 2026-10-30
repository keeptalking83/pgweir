// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/transparency-dev/merkle/testonly"
)

// Known-answer tests: RFC 6962 empty root and the Certificate Transparency
// test vectors shipped with transparency-dev/merkle.
func TestKnownAnswers(t *testing.T) {
	empty := sha256.Sum256(nil)
	root, err := NewTree(nil).Root(0)
	if err != nil || !bytes.Equal(root, empty[:]) {
		t.Fatalf("empty root %x, want %x", root, empty)
	}
	if !bytes.Equal(root, testonly.EmptyRootHash()) {
		t.Fatal("empty root differs from the reference vector")
	}

	inputs := testonly.LeafInputs()
	var leaves [][]byte
	for _, in := range inputs {
		leaves = append(leaves, hasher.HashLeaf(in))
	}
	// Leaf hash of the empty input: SHA256(0x00).
	if want := sha256.Sum256([]byte{0}); !bytes.Equal(leaves[0], want[:]) {
		t.Fatalf("leaf hash of empty input %x, want %x", leaves[0], want)
	}
	tree := NewTree(leaves)
	// RootHashes is indexed by tree size, starting with the empty tree.
	for size, want := range testonly.RootHashes() {
		got, err := tree.Root(uint64(size))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("root of size %d: %x, want %x", size, got, want)
		}
	}
}

func TestLeafHashIsCanonical(t *testing.T) {
	a := json.RawMessage(`{"b":1,"a":"x"}`)
	b := json.RawMessage(`{ "a" : "x", "b" : 1 }`)
	ca, ha, err := LeafHash(a)
	if err != nil {
		t.Fatal(err)
	}
	_, hb, _ := LeafHash(b)
	if !bytes.Equal(ha, hb) || string(ca) != `{"a":"x","b":1}` {
		t.Fatalf("canonicalization: %s %x %x", ca, ha, hb)
	}
	want := sha256.Sum256(append([]byte{0}, ca...))
	if !bytes.Equal(ha, want[:]) {
		t.Fatal("leaf hash is not SHA256(0x00 || jcs(record))")
	}
}

func randomLeaves(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = hasher.HashLeaf([]byte(fmt.Sprintf("leaf %d", i)))
	}
	return out
}

func TestInclusionAndConsistency(t *testing.T) {
	tree := NewTree(randomLeaves(70))
	for size := uint64(1); size <= 70; size++ {
		root, _ := tree.Root(size)
		for i := uint64(0); i < size; i += 7 {
			path, err := tree.Inclusion(i, size)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyInclusion(i, size, tree.leaves[i], path, root); err != nil {
				t.Fatalf("inclusion %d/%d: %v", i, size, err)
			}
			if i+1 < size {
				if err := VerifyInclusion(i, size, tree.leaves[i+1], path, root); err == nil {
					t.Fatalf("inclusion of the wrong leaf verified at %d/%d", i, size)
				}
			}
		}
		for s1 := uint64(1); s1 <= size; s1 += 5 {
			path, err := tree.Consistency(s1, size)
			if err != nil {
				t.Fatal(err)
			}
			r1, _ := tree.Root(s1)
			if err := VerifyConsistency(s1, size, path, r1, root); err != nil {
				t.Fatalf("consistency %d->%d: %v", s1, size, err)
			}
		}
	}
}

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func signedHead(t *testing.T, tree *Tree, size uint64, key ed25519.PrivateKey) SignedSTH {
	t.Helper()
	root, err := tree.Root(size)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public().(ed25519.PublicKey)
	sth := STH{V: 1, TreeSize: size, RootHash: hex.EncodeToString(root),
		Timestamp: FormatTime(time.Unix(1700000000, 0).Add(time.Duration(size) * time.Second)), KeyID: KeyID(pub)}
	sig, err := sth.Sign(key)
	if err != nil {
		t.Fatal(err)
	}
	return SignedSTH{STH: sth, Signature: hex.EncodeToString(sig)}
}

func makeProof(t *testing.T, records []json.RawMessage, index uint64, key ed25519.PrivateKey) *Proof {
	t.Helper()
	var leaves [][]byte
	for _, r := range records {
		_, h, err := LeafHash(r)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, h)
	}
	tree := NewTree(leaves)
	size := tree.Size()
	path, err := tree.Inclusion(index, size)
	if err != nil {
		t.Fatal(err)
	}
	head := signedHead(t, tree, size, key)
	return &Proof{LeafIndex: index, LeafRecord: records[index], LeafHash: hex.EncodeToString(leaves[index]),
		TreeSize: size, AuditPath: hexAll(path), STH: head.STH, Signature: head.Signature}
}

func TestVerifyProof(t *testing.T) {
	key := testKey(t)
	pub := key.Public().(ed25519.PublicKey)
	var records []json.RawMessage
	for i := 0; i < 13; i++ {
		records = append(records, json.RawMessage(fmt.Sprintf(`{"v":1,"merge_id":"m%d"}`, i)))
	}
	p := makeProof(t, records, 5, key)
	if err := VerifyProof(p, pub); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	tampered := *p
	tampered.LeafRecord = json.RawMessage(`{"v":1,"merge_id":"evil"}`)
	if err := VerifyProof(&tampered, pub); err == nil {
		t.Fatal("altered record accepted")
	}
	tampered = *p
	tampered.AuditPath = append([]string{}, p.AuditPath...)
	tampered.AuditPath[0] = hex.EncodeToString(make([]byte, 32))
	if err := VerifyProof(&tampered, pub); err == nil {
		t.Fatal("altered audit path accepted")
	}
	tampered = *p
	tampered.STH.Timestamp = "2030-01-01T00:00:00.000Z"
	if err := VerifyProof(&tampered, pub); err == nil {
		t.Fatal("altered tree head accepted")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if err := VerifyProof(p, other); err == nil {
		t.Fatal("proof verified with the wrong public key")
	}
}

func TestVerifyChain(t *testing.T) {
	key := testKey(t)
	pub := key.Public().(ed25519.PublicKey)
	tree := NewTree(randomLeaves(50))
	var heads []SignedSTH
	for size := uint64(1); size <= 50; size++ {
		heads = append(heads, signedHead(t, tree, size, key))
	}
	if err := VerifyChain(heads, pub, tree); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	// History rewritten: leaf 10 replaced, later heads re-signed.
	leaves := randomLeaves(50)
	leaves[10] = hasher.HashLeaf([]byte("rewritten"))
	if err := VerifyChain(heads, pub, NewTree(leaves)); err == nil {
		t.Fatal("rewritten history accepted")
	}
	// Truncated ledger.
	if err := VerifyChain(heads, pub, NewTree(randomLeaves(40))); err == nil {
		t.Fatal("truncated ledger accepted")
	}
	// Forged head.
	forged := heads[20]
	forged.STH.RootHash = hex.EncodeToString(make([]byte, 32))
	if err := VerifyChain(append(heads[:0:0], forged), pub, tree); err == nil {
		t.Fatal("forged head accepted")
	}
}

func TestConsistencyProofVerify(t *testing.T) {
	key := testKey(t)
	pub := key.Public().(ed25519.PublicKey)
	tree := NewTree(randomLeaves(20))
	path, err := tree.Consistency(7, 20)
	if err != nil {
		t.Fatal(err)
	}
	c := &ConsistencyProof{From: signedHead(t, tree, 7, key), To: signedHead(t, tree, 20, key), Proof: hexAll(path)}
	if err := c.Verify(pub); err != nil {
		t.Fatal(err)
	}
	c.From.STH.RootHash = hex.EncodeToString(make([]byte, 32))
	if err := c.Verify(nil); err == nil {
		t.Fatal("inconsistent roots accepted")
	}
}

func TestKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pgweir.key")
	pub, err := GenerateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(path); err == nil {
		t.Fatal("existing key overwritten")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", info.Mode().Perm())
	}
	priv, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("loaded key does not match")
	}
	if got, err := ParsePublicKey(hex.EncodeToString(pub)); err != nil || !got.Equal(pub) {
		t.Fatalf("parse hex: %v", err)
	}
	pubFile := filepath.Join(dir, "pgweir.pub")
	if err := os.WriteFile(pubFile, []byte(hex.EncodeToString(pub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ParsePublicKey(pubFile); err != nil || !got.Equal(pub) {
		t.Fatalf("parse file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(path); err == nil {
		t.Fatal("world-readable key accepted")
	}
	if len(KeyID(pub)) != 16 {
		t.Fatal("key id length")
	}
}
