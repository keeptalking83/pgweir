// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"fmt"
	"strconv"
)

// MinServerVersion is the oldest supported major version (pgoutput
// "messages" option).
const MinServerVersion = 140000

// ServerInfo describes the settings relevant to logical decoding and the
// privileges of the connected role.
type ServerInfo struct {
	VersionNum          int
	Version             string
	WALLevel            string
	MaxReplicationSlots int
	MaxWALSenders       int
	UsedSlots           int
	Role                string
	Superuser           bool
	Replication         bool
	CreateDB            bool
	Database            string
	OwnsDatabase        bool
	CanCreateInDB       bool
}

// Server reads ServerInfo from the connected server.
func Server(ctx context.Context, q Querier) (ServerInfo, error) {
	var s ServerInfo
	var versionNum, slots, senders string
	err := q.QueryRow(ctx, `
SELECT current_setting('server_version_num'),
       current_setting('server_version'),
       current_setting('wal_level'),
       current_setting('max_replication_slots'),
       current_setting('max_wal_senders'),
       (SELECT count(*) FROM pg_catalog.pg_replication_slots)::int,
       r.rolname, r.rolsuper, r.rolreplication, r.rolcreatedb,
       d.datname,
       pg_catalog.pg_has_role(d.datdba, 'MEMBER'),
       pg_catalog.has_database_privilege(d.datname, 'CREATE')
FROM pg_catalog.pg_roles r, pg_catalog.pg_database d
WHERE r.rolname = current_user AND d.datname = current_database()`).Scan(
		&versionNum, &s.Version, &s.WALLevel, &slots, &senders, &s.UsedSlots,
		&s.Role, &s.Superuser, &s.Replication, &s.CreateDB,
		&s.Database, &s.OwnsDatabase, &s.CanCreateInDB)
	if err != nil {
		return s, fmt.Errorf("read server settings: %w", err)
	}
	if s.VersionNum, err = strconv.Atoi(versionNum); err != nil {
		return s, fmt.Errorf("parse server_version_num %q: %w", versionNum, err)
	}
	if s.MaxReplicationSlots, err = strconv.Atoi(slots); err != nil {
		return s, fmt.Errorf("parse max_replication_slots %q: %w", slots, err)
	}
	if s.MaxWALSenders, err = strconv.Atoi(senders); err != nil {
		return s, fmt.Errorf("parse max_wal_senders %q: %w", senders, err)
	}
	return s, nil
}

// LogicalDecodingProblems lists the unmet server prerequisites for hosting a
// branch (spec §1.2, §1.3). An empty result means the server is ready.
func (s ServerInfo) LogicalDecodingProblems() []string {
	var p []string
	if s.VersionNum < MinServerVersion {
		p = append(p, fmt.Sprintf("Postgres %s is not supported: version 14 or later is required", s.Version))
	}
	if s.WALLevel != "logical" {
		p = append(p, fmt.Sprintf("wal_level is %q: set wal_level = logical and restart the server", s.WALLevel))
	}
	if s.MaxReplicationSlots < 1 {
		p = append(p, "max_replication_slots is 0: set it to at least the number of concurrently open branches")
	} else if s.UsedSlots >= s.MaxReplicationSlots {
		p = append(p, fmt.Sprintf("all %d replication slots are in use: raise max_replication_slots", s.MaxReplicationSlots))
	}
	if s.MaxWALSenders < 1 {
		p = append(p, "max_wal_senders is 0: set it to at least 1")
	}
	if !s.Replication && !s.Superuser {
		p = append(p, fmt.Sprintf("role %q lacks the REPLICATION attribute: ALTER ROLE %s REPLICATION", s.Role, Ident(s.Role)))
	}
	if !s.CanCreateInDB {
		p = append(p, fmt.Sprintf("role %q may not CREATE in database %q (needed for the publication)", s.Role, s.Database))
	}
	return p
}

// LocalProviderProblems lists the unmet prerequisites of the local provider
// (spec §5.1).
func (s ServerInfo) LocalProviderProblems() []string {
	var p []string
	if !s.CreateDB && !s.Superuser {
		p = append(p, fmt.Sprintf("role %q lacks CREATEDB: ALTER ROLE %s CREATEDB", s.Role, Ident(s.Role)))
	}
	if !s.OwnsDatabase && !s.Superuser {
		p = append(p, fmt.Sprintf("role %q does not own database %q: Postgres only lets the owner use it as a template", s.Role, s.Database))
	}
	return p
}
