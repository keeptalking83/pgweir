// SPDX-License-Identifier: Apache-2.0

// Package schemasql embeds the SQL files that install the pgweir metadata
// schema.
package schemasql

import _ "embed"

// Schema is the content of 001_schema.sql.
//
//go:embed 001_schema.sql
var Schema string
