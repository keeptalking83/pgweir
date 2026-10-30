// SPDX-License-Identifier: Apache-2.0

// Package meta holds the product name, the names derived from it, and build
// information. Renaming the product is a change to Name plus the constants
// below, the module path and the SQL schema file.
package meta

import "runtime/debug"

const (
	// Name is the product and binary name.
	Name = "pgweir"
	// Schema is the metadata schema installed on main.
	Schema = "pgweir"
	// Publication is the publication created on every branch.
	Publication = "pgweir"
	// SlotPrefix prefixes replication slot names on branches.
	SlotPrefix = "pw_"
	// DBPrefix prefixes database names created by the local provider.
	DBPrefix = "pw_"
	// EnvPrefix prefixes every environment variable pgweir reads.
	EnvPrefix = "PGWEIR_"
	// TagPrefix is the logical message prefix agents use to tag transactions.
	TagPrefix = "pgweir"
	// MergePrefix is the logical message prefix emitted on main by a merge.
	MergePrefix = "pgweir.merge"
	// SchemaVersion is the version of sql/001_schema.sql.
	SchemaVersion = 1
)

// Set at link time by the release build (-ldflags "-X ...").
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// BuildInfo returns version, commit and build date, falling back to the
// module build information embedded by the Go toolchain.
func BuildInfo() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	if info, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			}
		}
	}
	return version, commit, date
}
