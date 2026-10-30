// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
)

// UnverifiedBase is recorded in provider_ref.warning for manual forks: pgweir
// cannot verify that nothing was written between the fork and the slot.
const UnverifiedBase = "unverified_base"

// Manual uses a fork the user created by any means (spec §5.3).
type Manual struct {
	branchDSN string
}

func (m *Manual) Name() string { return "manual" }

func (m *Manual) Fork(_ context.Context, _ string, _ string) (string, map[string]any, error) {
	if m.branchDSN == "" {
		return "", nil, fmt.Errorf("the manual provider needs --branch-dsn")
	}
	return m.branchDSN, map[string]any{"warning": UnverifiedBase}, nil
}

// Drop does nothing: the user owns a manual fork.
func (m *Manual) Drop(context.Context, map[string]any) error { return nil }
