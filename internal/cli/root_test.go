// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, stderr: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "pgweir ") {
		t.Fatalf("unexpected stdout %q", stdout.String())
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"nope"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("expected non-zero exit code")
	}
}
