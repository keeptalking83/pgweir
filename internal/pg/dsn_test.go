// SPDX-License-Identifier: Apache-2.0

package pg

import "testing"

func TestWithDatabase(t *testing.T) {
	cases := []struct{ in, db, want string }{
		{"postgres://u:p@h:5432/shop?sslmode=disable", "pw_x", "postgres://u:p@h:5432/pw_x?sslmode=disable"},
		{"postgresql://u@h/shop", "pw_x", "postgresql://u@h/pw_x"},
		{"host=h user=u dbname=shop", "pw_x", "host=h user=u dbname=pw_x"},
		{"host=h user=u", "pw_x", "host=h user=u dbname=pw_x"},
		{"host=h password='a b\\'c' dbname=shop", "pw_x", `host=h password='a b\'c' dbname=pw_x`},
	}
	for _, c := range cases {
		got, err := WithDatabase(c.in, c.db)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("WithDatabase(%q) = %q, want %q", c.in, got, c.want)
		}
		db, err := DatabaseName(got)
		if err != nil || db != c.db {
			t.Errorf("DatabaseName(%q) = %q, %v", got, db, err)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"postgres://u:secret@h/db":       "postgres://u:xxxxx@h/db",
		"host=h password=secret user=u":  "host=h password=xxxxx user=u",
		"postgres://u@h/db?password=sec": "postgres://u@h/db?password=xxxxx",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseKeywordValueErrors(t *testing.T) {
	for _, in := range []string{"host", "host='x"} {
		if _, err := parseKeywordValue(in); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}
