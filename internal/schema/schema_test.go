// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"reflect"
	"testing"
)

func TestParseRefs(t *testing.T) {
	got, err := ParseRefs(" public.orders, customers ,public.orders,app.x")
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{{Schema: "public", Name: "orders"}, {Schema: "public", Name: "customers"}, {Schema: "app", Name: "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"", " , ", ".x", "x."} {
		if _, err := ParseRefs(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestDrift(t *testing.T) {
	main := &Table{PK: []string{"id"}, Columns: []Column{{Name: "id", Type: "integer"}, {Name: "s", Type: "public.order_status"}}}
	same := &Table{PK: []string{"id"}, Columns: []Column{{Name: "id", Type: "integer"}, {Name: "s", Type: "public.order_status"}}}
	if d := Drift(main, same); len(d) != 0 {
		t.Fatalf("unexpected drift %v", d)
	}
	changed := &Table{PK: []string{"id", "s"}, Columns: []Column{{Name: "id", Type: "bigint"}, {Name: "s", Type: "public.order_status"}, {Name: "x", Type: "text"}}}
	d := Drift(main, changed)
	if len(d) != 3 {
		t.Fatalf("want 3 differences, got %v", d)
	}
}

func TestProblems(t *testing.T) {
	ok := &Table{Kind: "r", Persistence: "p", PK: []string{"id"}, Owned: true}
	if p := ok.Problems(); len(p) != 0 {
		t.Fatalf("unexpected problems %v", p)
	}
	bad := &Table{Kind: "p", Persistence: "u"}
	if p := bad.Problems(); len(p) != 4 {
		t.Fatalf("want 4 problems, got %v", p)
	}
}
