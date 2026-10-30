// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keeptalking83/pgweir/internal/ledger"
	"github.com/keeptalking83/pgweir/internal/pg"
)

// TestLedgerFiftyMerges appends 50 merge records, exports in segments of
// 16, checks the export round-trips and that verify-chain accepts all 50
// signed tree heads.
func TestLedgerFiftyMerges(t *testing.T) {
	keyPath, pub := keygen(t)
	main := setup(t, "")
	key, err := ledger.LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pg.Connect(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	signer := &ledger.Signer{Key: key}
	for i := 0; i < 50; i++ {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = signer.Append(ctx, tx, ledger.Record{
			MergeID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), BranchID: "00000000-0000-4000-8000-000000000000",
			BranchName: "b", BaseLSN: "0/1", EndLSN: "0/2", MainLSNAfter: "0/3", ChangesetHash: "00", Policy: "FAIL",
			Stats: json.RawMessage(`{"rows_total":0}`), ConflictsHash: "00", Runs: []json.RawMessage{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	exp := t.TempDir()
	mustPgweir(t, "ledger", "export", "--dsn", main, "--dir", exp, "--segment", "16")
	files, _ := os.ReadDir(filepath.Join(exp, "leaves"))
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	want := "00000000-00000015.json,00000016-00000031.json,00000032-00000047.json,00000048-00000049.json"
	if strings.Join(names, ",") != want {
		t.Fatalf("leaf files %v", names)
	}

	// Round trip: recompute every leaf hash from the exported records and
	// the root from the exported hashes; it must equal the last tree head.
	var hashes [][]byte
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(exp, "leaves", n))
		if err != nil {
			t.Fatal(err)
		}
		var leaves []struct {
			LeafIndex uint64          `json:"leaf_index"`
			LeafHash  string          `json:"leaf_hash"`
			Record    json.RawMessage `json:"record"`
		}
		if err := json.Unmarshal(data, &leaves); err != nil {
			t.Fatal(err)
		}
		for _, l := range leaves {
			_, h, err := ledger.LeafHash(l.Record)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(h) != l.LeafHash || l.LeafIndex != uint64(len(hashes)) {
				t.Fatalf("leaf %d does not round-trip", l.LeafIndex)
			}
			hashes = append(hashes, h)
		}
	}
	root, err := ledger.NewTree(hashes).Root(50)
	if err != nil {
		t.Fatal(err)
	}
	last, err := os.ReadFile(filepath.Join(exp, "sth", "50.json"))
	if err != nil {
		t.Fatal(err)
	}
	var head ledger.SignedSTH
	if err := json.Unmarshal(last, &head); err != nil {
		t.Fatal(err)
	}
	if head.STH.RootHash != hex.EncodeToString(root) {
		t.Fatal("exported root does not match the recomputed root")
	}

	r := mustPgweir(t, "ledger", "verify-chain", "--dsn", main, "--sths", exp, "--pubkey", pub)
	if !strings.Contains(r.stdout, "50 signed tree heads are consistent") {
		t.Fatalf("verify-chain: %s", r.stdout)
	}
	// A JSON array works too, and a forged head is rejected.
	head.STH.RootHash = strings.Repeat("0", 64)
	arr, _ := json.Marshal([]ledger.SignedSTH{head})
	forged := filepath.Join(t.TempDir(), "sths.json")
	if err := os.WriteFile(forged, arr, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := pgweir(t, "ledger", "verify-chain", "--dsn", main, "--sths", forged, "--pubkey", pub); r.code != 1 {
		t.Fatalf("forged head accepted: %s", r.stdout)
	}
}
