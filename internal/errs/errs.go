// SPDX-License-Identifier: Apache-2.0

// Package errs defines the coded errors pgweir reports to users and the
// process exit codes they map to.
package errs

import (
	"errors"
	"fmt"
)

// Process exit codes (spec §10).
const (
	ExitOK           = 0
	ExitGeneric      = 1
	ExitNeedsConfirm = 2
	ExitRetry        = 3
	ExitPGAborted    = 4
	ExitConflicts    = 5
)

// Error codes used across packages.
const (
	SchemaDrift               = "SCHEMA_DRIFT"
	TruncateNotSupported      = "TRUNCATE_NOT_SUPPORTED"
	ToastWithoutOld           = "TOAST_WITHOUT_OLD"
	BinaryNotSupported        = "BINARY_NOT_SUPPORTED"
	ReplicaIdentityNotFull    = "REPLICA_IDENTITY_NOT_FULL"
	UntrackedTable            = "UNTRACKED_TABLE"
	InvalidTag                = "INVALID_TAG"
	SessionSettingsOverridden = "SESSION_SETTINGS_OVERRIDDEN"
	SequenceGapTooLarge       = "SEQUENCE_GAP_TOO_LARGE"
	SlotMissing               = "SLOT_MISSING"
	Retry                     = "RETRY"
	Conflicts                 = "CONFLICTS"
	PGAborted                 = "PG_ABORTED"
	NeedsConfirm              = "NEEDS_CONFIRMATION"
	NotInitialized            = "NOT_INITIALIZED"
	Prerequisite              = "PREREQUISITE"
	InvalidArgument           = "INVALID_ARGUMENT"
	BranchState               = "BRANCH_STATE"
	NoSigningKey              = "NO_SIGNING_KEY"
	ProofInvalid              = "PROOF_INVALID"
)

// Error is an error with a stable code and an exit code.
type Error struct {
	Code string
	Exit int
	Msg  string
	Err  error
	// Quiet: the command already reported the error on its own.
	Quiet bool
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// New returns an Error with exit code ExitGeneric unless the code implies another.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Exit: exitFor(code), Msg: fmt.Sprintf(format, args...)}
}

// Wrap returns an Error wrapping err.
func Wrap(code string, err error, format string, args ...any) *Error {
	return &Error{Code: code, Exit: exitFor(code), Msg: fmt.Sprintf(format, args...), Err: err}
}

func exitFor(code string) int {
	switch code {
	case Retry:
		return ExitRetry
	case PGAborted, SequenceGapTooLarge:
		return ExitPGAborted
	case Conflicts:
		return ExitConflicts
	case NeedsConfirm:
		return ExitNeedsConfirm
	default:
		return ExitGeneric
	}
}

// ExitCode returns the exit code for err (ExitOK for nil).
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Exit
	}
	return ExitGeneric
}

// HasCode reports whether err is an *Error with the given code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
