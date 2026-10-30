// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// GenerateKey writes a new ed25519 private key (64 bytes, seed || public
// key, hex encoded) to path with mode 0600 and returns the public key. An
// existing file is never overwritten.
func GenerateKey(path string) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path chosen by the operator
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(priv) + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return pub, nil
}

// LoadPrivateKey reads a key written by GenerateKey. It refuses files that
// group or others can read.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("signing key %s is readable by group or others (mode %o); run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- path chosen by the operator
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key %s is not a hex encoded 64-byte ed25519 private key", path)
	}
	key := ed25519.PrivateKey(raw)
	// The second half must be the public key of the seed.
	if !key.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(key.Seed()).Public()) {
		return nil, fmt.Errorf("signing key %s is corrupt: public half does not match the seed", path)
	}
	return key, nil
}

// ParsePublicKey accepts a hex encoded public key or the path of a file
// containing one.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	b, err := os.ReadFile(s) // #nosec G304 -- path chosen by the operator
	if err != nil {
		return nil, fmt.Errorf("public key is neither 64 hex characters nor a readable file: %w", err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s does not contain a hex encoded 32-byte ed25519 public key", s)
	}
	return ed25519.PublicKey(raw), nil
}
