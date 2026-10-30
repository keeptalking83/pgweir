// SPDX-License-Identifier: Apache-2.0

// Package config resolves pgweir's runtime configuration from flags and
// environment variables. There is no configuration file.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/keeptalking83/pgweir/internal/meta"
)

// Config is the global configuration shared by all commands.
type Config struct {
	MainDSN     string
	SigningKey  string
	IdleTimeout time.Duration
	LogLevel    string
	JSON        bool
}

// Env returns the value of the environment variable meta.EnvPrefix+name.
func Env(name string) string {
	return os.Getenv(meta.EnvPrefix + name)
}

// EnvDuration parses meta.EnvPrefix+name as a duration, or returns def.
func EnvDuration(name string, def time.Duration) time.Duration {
	if v := Env(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// SlogLevel converts LogLevel to a slog level.
func (c Config) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(c.LogLevel) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", c.LogLevel)
}

// RequireMainDSN returns an error when no main DSN is configured.
func (c Config) RequireMainDSN() error {
	if c.MainDSN == "" {
		return fmt.Errorf("no main database: pass --dsn or set %sMAIN_DSN", meta.EnvPrefix)
	}
	return nil
}
