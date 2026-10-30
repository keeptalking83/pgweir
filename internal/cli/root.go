// SPDX-License-Identifier: Apache-2.0

// Package cli implements the pgweir command line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/config"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
)

// app carries the configuration and output streams of one invocation.
// Human-readable output goes to stderr, machine output to stdout.
type app struct {
	cfg    config.Config
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	log    *slog.Logger
}

func (a *app) say(format string, args ...any) {
	fmt.Fprintf(a.stderr, format+"\n", args...)
}

func (a *app) emit(format string, args ...any) {
	fmt.Fprintf(a.stdout, format+"\n", args...)
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, stdin: stdin}
	root := newRoot(a)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stderr)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	var e *errs.Error
	quiet := errors.As(err, &e) && e.Quiet
	if err != nil && !quiet {
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
	return errs.ExitCode(err)
}

func newRoot(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   meta.Name,
		Short: "Branch, merge and audit agent writes to Postgres",
		Long: meta.Name + ` lets an AI agent write to a branch (a point-in-time fork of the main
database), captures every row the agent changed through logical replication,
merges those rows back into main with a row-level three-way merge, and records
each merge in a signed, tamper-evident Merkle ledger.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			lvl, err := a.cfg.SlogLevel()
			if err != nil {
				return err
			}
			a.log = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: lvl}))
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.cfg.MainDSN, "dsn", config.Env("MAIN_DSN"), "main database connection string (env "+meta.EnvPrefix+"MAIN_DSN)")
	pf.StringVar(&a.cfg.SigningKey, "signing-key", config.Env("SIGNING_KEY"), "path to the ed25519 ledger signing key (env "+meta.EnvPrefix+"SIGNING_KEY)")
	pf.DurationVar(&a.cfg.IdleTimeout, "idle-timeout", config.EnvDuration("IDLE_TIMEOUT", 2*time.Second), "capture stops after the branch has been idle this long past the target LSN")
	pf.StringVar(&a.cfg.LogLevel, "log-level", envDefault("LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	pf.BoolVar(&a.cfg.JSON, "json", false, "machine-readable JSON output on stdout")

	root.AddCommand(
		newInitCmd(a),
		newCheckCmd(a),
		newBranchCmd(a),
		newDiffCmd(a),
		newMergeCmd(a),
		newConflictsCmd(a),
		newProveCmd(a),
		newVerifyCmd(a),
		newLedgerCmd(a),
		newKeygenCmd(a),
		newVersionCmd(a),
	)
	return root
}

func envDefault(name, def string) string {
	if v := config.Env(name); v != "" {
		return v
	}
	return def
}
