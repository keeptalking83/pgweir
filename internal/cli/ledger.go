// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/ledger"
	"github.com/keeptalking83/pgweir/internal/meta"
)

func newKeygenCmd(a *app) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Create an ed25519 ledger signing key",
		Long: `Create an ed25519 signing key for the ledger. The private key is written
hex encoded to --out with mode 0600 and never overwritten; the public key is
printed on stdout, so "pgweir keygen --out pgweir.key > pgweir.pub" keeps both.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pub, err := ledger.GenerateKey(out)
			if err != nil {
				return err
			}
			a.emit("%s", hex.EncodeToString(pub))
			a.say("wrote private key to %s (mode 0600, key id %s); keep it secret and pass it with --signing-key or %sSIGNING_KEY",
				out, ledger.KeyID(pub), meta.EnvPrefix)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", meta.Name+".key", "path of the private key file")
	return cmd
}

func writeJSONOutput(a *app, out string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if out == "" {
		_, err = a.stdout.Write(data)
		return err
	}
	return writeFile(out, data)
}

func newProveCmd(a *app) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "prove <merge-id>",
		Short: "Write the inclusion proof of a merge against the latest signed tree head",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			p, err := ledger.Prove(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if err := writeJSONOutput(a, out, p); err != nil {
				return err
			}
			if out != "" {
				a.say("wrote proof of merge %s (leaf %d of %d) to %s", args[0], p.LeafIndex, p.TreeSize, out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the proof to this file instead of stdout")
	return cmd
}

func newVerifyCmd(a *app) *cobra.Command {
	var proofPath, pubKey string
	var trustEmbedded bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a proof offline",
		Long: `Verify a proof offline, without a database: the leaf hash of the record,
the inclusion path to the signed root, and the ed25519 signature with the given
public key. The public key embedded in the proof is ignored unless
--trust-embedded is set. Exit 0 when valid, 1 when invalid.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if proofPath == "" {
				return errs.New(errs.InvalidArgument, "pass --proof")
			}
			data, err := os.ReadFile(proofPath) // #nosec G304 -- path chosen by the user
			if err != nil {
				return err
			}
			var p ledger.Proof
			if err := json.Unmarshal(data, &p); err != nil {
				return a.invalid("proof is not valid JSON: %v", err)
			}
			var pub ed25519.PublicKey
			switch {
			case pubKey != "":
				if pub, err = ledger.ParsePublicKey(pubKey); err != nil {
					return errs.Wrap(errs.InvalidArgument, err, "public key")
				}
			case trustEmbedded && p.PublicKey != "":
				if pub, err = ledger.ParsePublicKey(p.PublicKey); err != nil {
					return a.invalid("embedded public key: %v", err)
				}
			default:
				return errs.New(errs.InvalidArgument, "pass --pubkey (or --trust-embedded to use the key inside the proof)")
			}
			if err := ledger.VerifyProof(&p, pub); err != nil {
				return a.invalid("%v", err)
			}
			var rec struct {
				MergeID string `json:"merge_id"`
			}
			_ = json.Unmarshal(p.LeafRecord, &rec)
			a.emit("valid: merge %s is leaf %d of the tree of size %d signed by key %s at %s",
				rec.MergeID, p.LeafIndex, p.TreeSize, p.STH.KeyID, p.STH.Timestamp)
			return nil
		},
	}
	cmd.Flags().StringVar(&proofPath, "proof", "", "proof file written by the prove command")
	cmd.Flags().StringVar(&pubKey, "pubkey", "", "ed25519 public key: 64 hex characters or a file containing them")
	cmd.Flags().BoolVar(&trustEmbedded, "trust-embedded", false, "use the public key embedded in the proof when --pubkey is not given")
	return cmd
}

func (a *app) invalid(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	a.emit("invalid: %s", msg)
	e := errs.New(errs.ProofInvalid, "%s", msg)
	e.Quiet = true
	return e
}

func newLedgerCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "Inspect, verify and export the merge ledger",
	}
	cmd.AddCommand(newLedgerSTHCmd(a), newLedgerConsistencyCmd(a), newLedgerVerifyChainCmd(a), newLedgerExportCmd(a))
	return cmd
}

func newLedgerSTHCmd(a *app) *cobra.Command {
	var at uint64
	cmd := &cobra.Command{
		Use:   "sth",
		Short: "Print the latest signed tree head, or the one of a given size",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			s, err := ledger.LoadSTH(ctx, conn, at)
			if err != nil {
				return err
			}
			return writeJSONOutput(a, "", s)
		},
	}
	cmd.Flags().Uint64Var(&at, "at", 0, "tree size (default: latest)")
	return cmd
}

func newLedgerConsistencyCmd(a *app) *cobra.Command {
	var from, to uint64
	cmd := &cobra.Command{
		Use:   "consistency",
		Short: "Print a consistency proof between two tree sizes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			if to == 0 {
				head, err := ledger.LoadSTH(ctx, conn, 0)
				if err != nil {
					return err
				}
				to = head.STH.TreeSize
			}
			c, err := ledger.Consistency(ctx, conn, from, to)
			if err != nil {
				return err
			}
			return writeJSONOutput(a, "", c)
		},
	}
	cmd.Flags().Uint64Var(&from, "from", 0, "older tree size")
	cmd.Flags().Uint64Var(&to, "to", 0, "newer tree size (default: latest)")
	return cmd
}

func newLedgerVerifyChainCmd(a *app) *cobra.Command {
	var sthsPath string
	var pubKeys []string
	cmd := &cobra.Command{
		Use:   "verify-chain",
		Short: "Verify that previously published tree heads are prefixes of the current ledger",
		Long: `Verify a set of previously published signed tree heads (for example the
sth/ objects of an export): every signature must verify with one of the
--pubkey keys (repeat the flag after a key rotation), every tree head must be an
append-only prefix of the ledger now on main, and every leaf record on main must
match its leaf hash. A rewritten or truncated ledger fails. --sths is a JSON
array of signed tree heads or a directory of sth/<size>.json files.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if sthsPath == "" || len(pubKeys) == 0 {
				return errs.New(errs.InvalidArgument, "pass --sths and --pubkey")
			}
			var pubs []ed25519.PublicKey
			for _, k := range pubKeys {
				pub, err := ledger.ParsePublicKey(k)
				if err != nil {
					return errs.Wrap(errs.InvalidArgument, err, "public key")
				}
				pubs = append(pubs, pub)
			}
			heads, err := readSTHs(sthsPath)
			if err != nil {
				return err
			}
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			tree, err := ledger.LoadTreeChecked(ctx, conn)
			if err != nil {
				return a.invalid("%v", err)
			}
			if err := ledger.VerifyChain(heads, pubs, tree); err != nil {
				return a.invalid("%v", err)
			}
			a.emit("valid: %s are consistent with the current ledger (%d leaves)", plural(len(heads), "signed tree head"), tree.Size())
			return nil
		},
	}
	cmd.Flags().StringVar(&sthsPath, "sths", "", "JSON array of signed tree heads, or a directory of sth/<size>.json files")
	cmd.Flags().StringArrayVar(&pubKeys, "pubkey", nil, "ed25519 public key: 64 hex characters or a file containing them; repeat after a key rotation")
	return cmd
}

func readSTHs(p string) ([]ledger.SignedSTH, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(p) // #nosec G304 -- path chosen by the user
		if err != nil {
			return nil, err
		}
		var heads []ledger.SignedSTH
		if err := json.Unmarshal(data, &heads); err != nil {
			return nil, errs.Wrap(errs.InvalidArgument, err, "%s is not a JSON array of signed tree heads", p)
		}
		return heads, nil
	}
	if sub := filepath.Join(p, "sth"); dirExists(sub) {
		p = sub
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var heads []ledger.SignedSTH
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p, e.Name())) // #nosec G304 -- inside the given directory
		if err != nil {
			return nil, err
		}
		var h ledger.SignedSTH
		if err := json.Unmarshal(data, &h); err != nil {
			return nil, errs.Wrap(errs.InvalidArgument, err, "%s", e.Name())
		}
		heads = append(heads, h)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].STH.TreeSize < heads[j].STH.TreeSize })
	return heads, nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func newLedgerExportCmd(a *app) *cobra.Command {
	var dir, s3URL string
	var segment int
	var includeChangesets bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export ledger leaves and signed tree heads to a directory or S3",
		Long: `Export the ledger: leaves/<first>-<last>.json, sth/<tree_size>.json and,
with --include-changesets, changesets/<merge_id>.json. Objects that already
exist are never overwritten; an existing object with different content is an
error, which means the ledger changed after it was exported.

For S3, enable Object Lock (compliance mode) on the bucket yourself; pgweir
does not configure buckets.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if (dir == "") == (s3URL == "") {
				return errs.New(errs.InvalidArgument, "pass exactly one of --dir or --s3")
			}
			var sink ledger.Sink
			if dir != "" {
				sink = ledger.DirSink{Root: dir}
			} else {
				s, err := ledger.NewS3Sink(ctx, s3URL)
				if err != nil {
					return errs.Wrap(errs.InvalidArgument, err, "s3")
				}
				sink = s
			}
			conn, err := a.connectMain(ctx)
			if err != nil {
				return err
			}
			defer closeConn(ctx, conn)
			st, err := ledger.Export(ctx, conn, sink, ledger.ExportOptions{Segment: segment, IncludeChangesets: includeChangesets})
			if err != nil {
				return err
			}
			a.say("exported to %s: %d leaves in %d new leaf files, %d new tree heads, %d new changesets",
				sink, st.Leaves, st.LeafFiles, st.STHFiles, st.Changesets)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "export to this directory")
	cmd.Flags().StringVar(&s3URL, "s3", "", "export to s3://bucket/prefix")
	cmd.Flags().IntVar(&segment, "segment", 1000, "maximum leaves per leaves/ file")
	cmd.Flags().BoolVar(&includeChangesets, "include-changesets", false, "also export the canonical changeset of every applied merge")
	return cmd
}
