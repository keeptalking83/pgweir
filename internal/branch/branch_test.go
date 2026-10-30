// SPDX-License-Identifier: Apache-2.0

package branch

import (
	"regexp"
	"testing"

	"github.com/keeptalking83/pgweir/internal/schema"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"demo", "a", "agent_run_42", "x123456789012345678901234567890123456789012345678"[:48]} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Demo", "1abc", "has-dash", "has space", "x1234567890123456789012345678901234567890123456789"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if !re.MatchString(id) {
			t.Fatalf("bad uuid %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid %q", id)
		}
		seen[id] = true
	}
}

func TestSameRefs(t *testing.T) {
	a := []schema.Ref{{Schema: "public", Name: "a"}, {Schema: "public", Name: "b"}}
	b := []schema.Ref{{Schema: "public", Name: "b"}, {Schema: "public", Name: "a"}}
	if !sameRefs(a, b) {
		t.Fatal("expected equal")
	}
	if sameRefs(a, b[:1]) {
		t.Fatal("expected different")
	}
}
