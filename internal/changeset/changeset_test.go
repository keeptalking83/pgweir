// SPDX-License-Identifier: Apache-2.0

package changeset

import (
	"bytes"
	"encoding/json"
	"testing"
)

func row(kv ...string) Row {
	r := Row{}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "<null>" {
			r[kv[i]] = nil
		} else {
			r[kv[i]] = Str(kv[i+1])
		}
	}
	return r
}

func tag(run string) json.RawMessage {
	return json.RawMessage(`{"run_id":"` + run + `"}`)
}

func TestRowEqual(t *testing.T) {
	a := row("id", "1", "x", "<null>")
	if !a.Equal(row("id", "1", "x", "<null>")) {
		t.Fatal("equal rows reported different")
	}
	if a.Equal(row("id", "1", "x", "")) {
		t.Fatal("NULL equals empty string")
	}
	if a.Equal(row("id", "1")) {
		t.Fatal("different column sets reported equal")
	}
	var n Row
	if !n.Equal(nil) || n.Equal(Row{}) {
		t.Fatal("nil row equality")
	}
}

func TestCollapse(t *testing.T) {
	events := []Event{
		{Seq: 1, Op: OpUpdate, Schema: "public", Table: "t", PK: row("id", "1"), Old: row("id", "1", "v", "a"), New: row("id", "1", "v", "b"), Run: tag("r1")},
		{Seq: 2, Op: OpInsert, Schema: "public", Table: "t", PK: row("id", "2"), New: row("id", "2", "v", "x"), Run: tag("r1")},
		{Seq: 3, Op: OpUpdate, Schema: "public", Table: "t", PK: row("id", "1"), Old: row("id", "1", "v", "b"), New: row("id", "1", "v", "c"), Run: tag("r2")},
		{Seq: 4, Op: OpDelete, Schema: "public", Table: "t", PK: row("id", "2"), Old: row("id", "2", "v", "x"), Run: tag("r2")},
		{Seq: 5, Op: OpDelete, Schema: "public", Table: "t", PK: row("id", "10"), Old: row("id", "10", "v", "z"), Run: json.RawMessage(`{"run_id":"untagged"}`)},
		{Seq: 6, Op: OpUpdate, Schema: "public", Table: "a", PK: row("id", "1"), Old: row("id", "1", "v", "p"), New: row("id", "1", "v", "q"), Run: tag("r1")},
	}
	got := Collapse(events)
	if len(got) != 3 {
		t.Fatalf("want 3 rows (insert+delete dropped), got %d: %+v", len(got), got)
	}
	// sorted by schema, table, canonical pk: a/1, t/{"id":"1"}, t/{"id":"10"}
	if got[0].Table != "a" || *got[1].PK["id"] != "1" || *got[2].PK["id"] != "10" {
		t.Fatalf("unexpected order: %+v", got)
	}
	r := got[1]
	if *r.Base["v"] != "a" || *r.Final["v"] != "c" || r.Ops != 2 || r.FirstSeq != 1 || r.LastSeq != 3 {
		t.Fatalf("bad collapse: %+v", r)
	}
	if len(r.Runs) != 2 || r.Runs[0] != "r1" || r.Runs[1] != "r2" {
		t.Fatalf("bad runs %v", r.Runs)
	}
	if got[2].Final != nil || got[2].Op() != OpDelete || got[1].Op() != OpUpdate {
		t.Fatal("bad op classification")
	}
}

func TestCanonicalChangesetIsStable(t *testing.T) {
	events := []Event{
		{Seq: 2, Op: OpInsert, Schema: "public", Table: "t", PK: row("id", "2"), New: row("id", "2", "v", "<x&y>"), Run: json.RawMessage(`{"agent":"bot","run_id":"r1","tool_call_id":"c1"}`)},
		{Seq: 1, Op: OpUpdate, Schema: "public", Table: "t", PK: row("id", "1"), Old: row("id", "1", "v", "a"), New: row("id", "1", "v", "é"), Run: json.RawMessage(`{"run_id":"r0"}`)},
	}
	tables := []TableDef{{Schema: "public", Name: "t", PK: []string{"id"}, Columns: []ColumnDef{{Name: "id", Type: "integer"}, {Name: "v", Type: "text"}}}}
	m := Meta{BranchID: "b", BranchName: "demo", BaseLSN: "0/1", EndLSN: "0/2"}
	a, err := Build(m, tables, events)
	if err != nil {
		t.Fatal(err)
	}
	// Same events in another order must give byte-identical output.
	b, err := Build(m, tables, []Event{events[1], events[0]})
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := a.Canonical()
	cb, _ := b.Canonical()
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical JSON differs:\n%s\n%s", ca, cb)
	}
	want := `{"base_lsn":"0/1","branch_id":"b","branch_name":"demo","end_lsn":"0/2",` +
		`"rows":[{"base":{"id":"1","v":"a"},"final":{"id":"1","v":"é"},"ops":1,"pk":{"id":"1"},"runs":["r0"],"schema":"public","table":"t"},` +
		`{"base":null,"final":{"id":"2","v":"<x&y>"},"ops":1,"pk":{"id":"2"},"runs":["r1"],"schema":"public","table":"t"}],` +
		`"runs":[{"agent":null,"parent_run_id":null,"run_id":"r0","tool_call_id":null},{"agent":"bot","parent_run_id":null,"run_id":"r1","tool_call_id":"c1"}],` +
		`"tables":[{"columns":[{"name":"id","type":"integer"},{"name":"v","type":"text"}],"name":"t","pk":["id"],"schema":"public"}],"version":1}`
	if string(ca) != want {
		t.Fatalf("canonical JSON:\n got %s\nwant %s", ca, want)
	}
	h1, _ := a.Hash()
	h2, _ := b.Hash()
	if !bytes.Equal(h1, h2) || len(h1) != 32 {
		t.Fatal("hash not stable")
	}
}

func TestFilter(t *testing.T) {
	events := []Event{
		{Seq: 1, Op: OpInsert, Schema: "public", Table: "a", PK: row("id", "1"), New: row("id", "1"), Run: tag("r1")},
		{Seq: 2, Op: OpInsert, Schema: "public", Table: "b", PK: row("id", "1"), New: row("id", "1"), Run: tag("r2")},
	}
	cs, err := Build(Meta{}, nil, events)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cs.Filter("public.a", "").Rows); n != 1 {
		t.Fatalf("table filter: %d rows", n)
	}
	if n := len(cs.Filter("", "r2").Rows); n != 1 {
		t.Fatalf("run filter: %d rows", n)
	}
	if n := len(cs.Filter("public.a", "r2").Rows); n != 0 {
		t.Fatalf("combined filter: %d rows", n)
	}
}
