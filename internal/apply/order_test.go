// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"reflect"
	"testing"

	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/schema"
)

func ref(name string) schema.Ref { return schema.Ref{Schema: "public", Name: name} }

func TestTableOrder(t *testing.T) {
	tables := []schema.Ref{ref("order_items"), ref("orders"), ref("customers"), ref("audit")}
	edges := []schema.FKEdge{
		{Child: ref("orders"), Parent: ref("customers")},
		{Child: ref("order_items"), Parent: ref("orders")},
		{Child: ref("customers"), Parent: ref("customers")}, // self-reference
	}
	got := TableOrder(tables, edges)
	want := []schema.Ref{ref("audit"), ref("customers"), ref("orders"), ref("order_items")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTableOrderCycle(t *testing.T) {
	tables := []schema.Ref{ref("b"), ref("a"), ref("c")}
	edges := []schema.FKEdge{
		{Child: ref("a"), Parent: ref("b")},
		{Child: ref("b"), Parent: ref("a")},
		{Child: ref("c"), Parent: ref("a")},
	}
	got := TableOrder(tables, edges)
	want := []schema.Ref{ref("a"), ref("b"), ref("c")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderActions(t *testing.T) {
	order := []schema.Ref{ref("customers"), ref("orders")}
	act := func(kind merge.ActionKind, table string, first, last int64) merge.Action {
		return merge.Action{Kind: kind, Schema: "public", Table: table, FirstSeq: first, LastSeq: last}
	}
	actions := []merge.Action{
		act(merge.Delete, "customers", 1, 9), // old parent deleted last on the branch
		act(merge.Update, "orders", 2, 8),    // child re-pointed to the new parent
		act(merge.Insert, "customers", 5, 5), // new parent
		act(merge.Delete, "orders", 3, 4),
		act(merge.Insert, "orders", 1, 1),
	}
	got := OrderActions(actions, order)
	want := []merge.Action{
		act(merge.Insert, "customers", 5, 5),
		act(merge.Insert, "orders", 1, 1),
		act(merge.Update, "orders", 2, 8),
		act(merge.Delete, "orders", 3, 4),
		act(merge.Delete, "customers", 1, 9),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
