// SPDX-License-Identifier: Apache-2.0

// Command pgweir lets AI agents write to a Postgres branch and merges their
// row changes back into main through a three-way merge recorded in a signed
// Merkle ledger.
package main

import (
	"os"

	"github.com/keeptalking83/pgweir/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
