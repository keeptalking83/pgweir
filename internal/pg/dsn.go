// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// DatabaseName returns the database a connection string points at.
func DatabaseName(dsn string) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse connection string: %w", err)
	}
	return cfg.Database, nil
}

// UserName returns the role a connection string authenticates as.
func UserName(dsn string) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse connection string: %w", err)
	}
	return cfg.User, nil
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// WithDatabase returns dsn with its database name replaced by db. Both URL
// and keyword/value connection strings are supported.
func WithDatabase(dsn, db string) (string, error) {
	if isURL(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse connection URL: %w", err)
		}
		u.Path = "/" + db
		u.RawPath = ""
		q := u.Query()
		if q.Has("dbname") {
			q.Set("dbname", db)
			u.RawQuery = q.Encode()
		}
		return u.String(), nil
	}
	pairs, err := parseKeywordValue(dsn)
	if err != nil {
		return "", err
	}
	found := false
	for i := range pairs {
		if pairs[i][0] == "dbname" {
			pairs[i][1] = db
			found = true
		}
	}
	if !found {
		pairs = append(pairs, [2]string{"dbname", db})
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p[0] + "=" + quoteKV(p[1])
	}
	return strings.Join(parts, " "), nil
}

// Redact hides the password of a connection string for log output.
func Redact(dsn string) string {
	if isURL(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return "<unparseable connection string>"
		}
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
		q := u.Query()
		if q.Has("password") {
			q.Set("password", "xxxxx")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}
	pairs, err := parseKeywordValue(dsn)
	if err != nil {
		return "<unparseable connection string>"
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		v := p[1]
		if p[0] == "password" {
			v = "xxxxx"
		}
		parts[i] = p[0] + "=" + quoteKV(v)
	}
	return strings.Join(parts, " ")
}

func quoteKV(v string) string {
	if v != "" && !strings.ContainsAny(v, " '\\\t\n") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// parseKeywordValue parses a libpq keyword/value connection string into
// ordered pairs.
func parseKeywordValue(s string) ([][2]string, error) {
	var pairs [][2]string
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return pairs, nil
		}
		start := i
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		key := s[start:i]
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return nil, fmt.Errorf("invalid connection string: missing '=' after %q", key)
		}
		i++
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("invalid connection string: unterminated quote for %q", key)
			}
		} else {
			for i < len(s) && !isSpace(s[i]) {
				if s[i] == '\\' && i+1 < len(s) {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				val.WriteByte(s[i])
				i++
			}
		}
		pairs = append(pairs, [2]string{key, val.String()})
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
