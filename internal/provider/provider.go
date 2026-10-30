// SPDX-License-Identifier: Apache-2.0

// Package provider creates and destroys forks of the main database.
package provider

import (
	"context"
	"fmt"
	"log/slog"
)

// Provider creates point-in-time forks of main.
type Provider interface {
	// Fork creates a point-in-time copy of main and returns a DSN for it plus provider refs.
	// MUST NOT return until the fork accepts connections.
	Fork(ctx context.Context, mainDSN string, name string) (branchDSN string, ref map[string]any, err error)
	// Drop destroys the fork.
	Drop(ctx context.Context, ref map[string]any) error
	Name() string
}

// Options configure a provider.
type Options struct {
	// MainDSN is the main connection string (local provider: Drop connects
	// to the same server).
	MainDSN string
	// BranchDSN is the fork the user created (manual provider).
	BranchDSN string
	// NeonParent is the Neon parent branch ID or name (neon provider).
	NeonParent string
	Log        *slog.Logger
}

// Names lists the providers of this version.
var Names = []string{"local", "neon", "manual"}

// New returns the named provider.
func New(name string, opts Options) (Provider, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	switch name {
	case "local":
		return &Local{mainDSN: opts.MainDSN, log: opts.Log}, nil
	case "neon":
		return NewNeon(opts)
	case "manual":
		return &Manual{branchDSN: opts.BranchDSN}, nil
	}
	return nil, fmt.Errorf("unknown provider %q (want local, neon or manual)", name)
}

// Owned reports whether Drop destroys the fork (false for manual forks,
// which the user manages).
func Owned(p Provider) bool {
	_, manual := p.(*Manual)
	return !manual
}
