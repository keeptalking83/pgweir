// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/keeptalking83/pgweir/internal/changeset"
)

type R = changeset.Row

// row builds a row from "col=value" pairs; "col=~" is NULL.
func row(pairs ...string) R {
	r := R{}
	for _, p := range pairs {
		var k, v string
		for i := 0; i < len(p); i++ {
			if p[i] == '=' {
				k, v = p[:i], p[i+1:]
				break
			}
		}
		if v == "~" {
			r[k] = nil
		} else {
			r[k] = changeset.Str(v)
		}
	}
	return r
}

func change(base, final R) changeset.RowChange {
	return changeset.RowChange{Schema: "public", Table: "t", PK: row("id=1"), Base: base, Final: final, FirstSeq: 1, LastSeq: 2}
}

func compute(policy Policy, rc changeset.RowChange, ours R) Plan {
	in := Input{Rows: []changeset.RowChange{rc}, Ours: map[string]R{}, Policy: policy}
	if ours != nil {
		in.Ours[OursKey(rc.Schema, rc.Table, rc.PK)] = ours
	}
	return Compute(in)
}

// TestDecisionTable covers every row of spec §9.2 under every policy.
func TestDecisionTable(t *testing.T) {
	B := row("id=1", "a=1", "b=1")
	T := row("id=1", "a=2", "b=1")      // branch changed a
	Odisj := row("id=1", "a=1", "b=9")  // main changed b
	Osame := row("id=1", "a=7", "b=1")  // main changed a too
	Tnull := row("id=1", "a=~", "b=1")  // branch set a to NULL
	Oempty := row("id=1", "a=", "b=1")  // main set a to ''
	Tins := row("id=1", "a=5", "b=5")   // branch insert
	Oins := row("id=1", "a=6", "b=6")   // main insert with same key
	Oequal := row("id=1", "a=5", "b=5") // main already holds the branch row

	type want struct {
		kind       ActionKind // "" = no action
		row        R
		expect     R
		conflict   Kind
		resolution Resolution
	}
	cases := []struct {
		name  string
		base  R
		final R
		ours  R
		fail  want
		skip  want
		acc   want
	}{
		{name: "insert, absent on main", base: nil, final: Tins, ours: nil,
			fail: want{kind: Insert, row: Tins}, skip: want{kind: Insert, row: Tins}, acc: want{kind: Insert, row: Tins}},
		{name: "insert, main has equal row", base: nil, final: Tins, ours: Oequal},
		{name: "insert, main has different row", base: nil, final: Tins, ours: Oins,
			fail: want{conflict: InsertInsert, resolution: Unresolved},
			skip: want{conflict: InsertInsert, resolution: Skipped},
			acc:  want{kind: Update, row: Tins, expect: Oins, conflict: InsertInsert, resolution: AcceptedTheirs}},
		{name: "delete, already gone", base: B, final: nil, ours: nil},
		{name: "delete, main unchanged", base: B, final: nil, ours: B,
			fail: want{kind: Delete, expect: B}, skip: want{kind: Delete, expect: B}, acc: want{kind: Delete, expect: B}},
		{name: "delete, main modified", base: B, final: nil, ours: Odisj,
			fail: want{conflict: DeleteModify, resolution: Unresolved},
			skip: want{conflict: DeleteModify, resolution: Skipped},
			acc:  want{kind: Delete, expect: Odisj, conflict: DeleteModify, resolution: AcceptedTheirs}},
		{name: "modify, main deleted", base: B, final: T, ours: nil,
			fail: want{conflict: ModifyDelete, resolution: Unresolved},
			skip: want{conflict: ModifyDelete, resolution: Skipped},
			acc:  want{kind: Insert, row: T, conflict: ModifyDelete, resolution: AcceptedTheirs}},
		{name: "modify, main unchanged", base: B, final: T, ours: B,
			fail: want{kind: Update, row: T, expect: B}, skip: want{kind: Update, row: T, expect: B}, acc: want{kind: Update, row: T, expect: B}},
		{name: "modify, disjoint cells", base: B, final: T, ours: Odisj,
			fail: want{kind: Update, row: row("id=1", "a=2", "b=9"), expect: Odisj},
			skip: want{kind: Update, row: row("id=1", "a=2", "b=9"), expect: Odisj},
			acc:  want{kind: Update, row: row("id=1", "a=2", "b=9"), expect: Odisj}},
		// Main already made the branch's change: merged row equals main's, no action.
		{name: "modify, same cell same value", base: B, final: T, ours: row("id=1", "a=2", "b=9")},
		{name: "modify, same cell different value", base: B, final: T, ours: Osame,
			fail: want{conflict: CellConflict, resolution: Unresolved},
			skip: want{conflict: CellConflict, resolution: Skipped},
			acc:  want{kind: Update, row: row("id=1", "a=2", "b=1"), expect: Osame, conflict: CellConflict, resolution: AcceptedTheirs}},
		{name: "NULL is not empty string", base: B, final: Tnull, ours: Oempty,
			fail: want{conflict: CellConflict, resolution: Unresolved},
			skip: want{conflict: CellConflict, resolution: Skipped},
			acc:  want{kind: Update, row: Tnull, expect: Oempty, conflict: CellConflict, resolution: AcceptedTheirs}},
	}
	for _, c := range cases {
		for _, pw := range []struct {
			policy Policy
			w      want
		}{{PolicyFail, c.fail}, {PolicySkip, c.skip}, {PolicyAccept, c.acc}} {
			t.Run(c.name+"/"+string(pw.policy), func(t *testing.T) {
				p := compute(pw.policy, change(c.base, c.final), c.ours)
				w := pw.w
				if w.kind == "" {
					if len(p.Actions) != 0 {
						t.Fatalf("want no action, got %+v", p.Actions)
					}
					if p.Stats.Skipped != 1 {
						t.Fatalf("stats %+v", p.Stats)
					}
				} else {
					if len(p.Actions) != 1 {
						t.Fatalf("want one %s, got %+v", w.kind, p.Actions)
					}
					a := p.Actions[0]
					if a.Kind != w.kind || !a.Row.Equal(w.row) || !a.ExpectOurs.Equal(w.expect) {
						t.Fatalf("got %s row=%v expect=%v, want %s row=%v expect=%v", a.Kind, a.Row, a.ExpectOurs, w.kind, w.row, w.expect)
					}
				}
				if w.conflict == "" {
					if len(p.Conflicts) != 0 {
						t.Fatalf("unexpected conflict %+v", p.Conflicts)
					}
				} else {
					if len(p.Conflicts) != 1 || p.Conflicts[0].Kind != w.conflict || p.Conflicts[0].Resolution != w.resolution {
						t.Fatalf("want %s/%s, got %+v", w.conflict, w.resolution, p.Conflicts)
					}
				}
				wantStatus := StatusComputed
				if pw.policy == PolicyFail && w.conflict != "" {
					wantStatus = StatusConflict
				}
				if p.Status != wantStatus {
					t.Fatalf("status %s, want %s", p.Status, wantStatus)
				}
			})
		}
	}
}

func TestCellConflictCells(t *testing.T) {
	B := row("id=1", "a=1", "b=1", "c=1")
	T := row("id=1", "a=2", "b=2", "c=1")
	O := row("id=1", "a=3", "b=1", "c=3")
	p := compute(PolicyFail, change(B, T), O)
	if len(p.Conflicts) != 1 {
		t.Fatalf("%+v", p.Conflicts)
	}
	cells := p.Conflicts[0].Cells
	if len(cells) != 1 || cells[0].Column != "a" || *cells[0].Base != "1" || *cells[0].Ours != "3" || *cells[0].Theirs != "2" {
		t.Fatalf("cells %+v", cells)
	}
	// ACCEPT: O with T for all changed columns (a and b), c stays main's.
	p = compute(PolicyAccept, change(B, T), O)
	if want := row("id=1", "a=2", "b=2", "c=3"); !p.Actions[0].Row.Equal(want) {
		t.Fatalf("accept row %v, want %v", p.Actions[0].Row, want)
	}
}

func TestSequenceWarning(t *testing.T) {
	rc := change(nil, row("id=1", "a=1"))
	in := Input{Rows: []changeset.RowChange{rc}, Policy: PolicyAccept, SequencePK: map[string]bool{"public.t": true},
		Ours: map[string]R{OursKey("public", "t", rc.PK): row("id=1", "a=2")}}
	if p := Compute(in); len(p.Warnings) != 1 {
		t.Fatalf("want a warning, got %v", p.Warnings)
	}
	in.Policy = PolicySkip
	if p := Compute(in); len(p.Warnings) != 0 {
		t.Fatalf("unexpected warning %v", p.Warnings)
	}
}

func TestParsePolicy(t *testing.T) {
	for in, want := range map[string]Policy{"fail": PolicyFail, "SKIP": PolicySkip, " Accept ": PolicyAccept} {
		if got, err := ParsePolicy(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParsePolicy("theirs"); err == nil {
		t.Error("expected error")
	}
}

// --- property tests ---

func randomRow(rng *rand.Rand, id int) R {
	r := R{"id": changeset.Str(fmt.Sprint(id))}
	for _, c := range []string{"a", "b", "c"} {
		switch rng.IntN(4) {
		case 0:
			r[c] = nil
		default:
			r[c] = changeset.Str(fmt.Sprint(rng.IntN(3)))
		}
	}
	return r
}

func randomChanges(rng *rand.Rand, n int) []changeset.RowChange {
	var out []changeset.RowChange
	for id := 0; id < n; id++ {
		var base, final R
		switch rng.IntN(3) {
		case 0:
			final = randomRow(rng, id)
		case 1:
			base = randomRow(rng, id)
		default:
			base, final = randomRow(rng, id), randomRow(rng, id)
		}
		out = append(out, changeset.RowChange{Schema: "public", Table: "t", PK: R{"id": changeset.Str(fmt.Sprint(id))}, Base: base, Final: final})
	}
	return out
}

// applyModel applies a plan to an in-memory main, re-verifying ExpectOurs
// like apply does.
func applyModel(t *testing.T, model map[string]R, p Plan) {
	t.Helper()
	for _, a := range p.Actions {
		k := OursKey(a.Schema, a.Table, a.PK)
		if cur := model[k]; !cur.Equal(a.ExpectOurs) {
			t.Fatalf("expect_ours mismatch for %v: model %v, expect %v", a.PK, cur, a.ExpectOurs)
		}
		switch a.Kind {
		case Insert, Update:
			model[k] = a.Row
		case Delete:
			delete(model, k)
		}
	}
}

// With main unchanged since the fork (O == B), applying the plan yields
// exactly the branch's final rows.
func TestPropertyUnchangedMainYieldsFinal(t *testing.T) {
	for seed := uint64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 1))
		rows := randomChanges(rng, 20)
		model := map[string]R{}
		for _, rc := range rows {
			if rc.Base != nil {
				model[OursKey(rc.Schema, rc.Table, rc.PK)] = rc.Base
			}
		}
		for _, policy := range []Policy{PolicyFail, PolicySkip, PolicyAccept} {
			ours := map[string]R{}
			for k, v := range model {
				ours[k] = v
			}
			p := Compute(Input{Rows: rows, Ours: ours, Policy: policy})
			if len(p.Conflicts) != 0 {
				t.Fatalf("seed %d: conflicts with an unchanged main: %+v", seed, p.Conflicts)
			}
			m := map[string]R{}
			for k, v := range ours {
				m[k] = v
			}
			applyModel(t, m, p)
			for _, rc := range rows {
				if got := m[OursKey(rc.Schema, rc.Table, rc.PK)]; !got.Equal(rc.Final) {
					t.Fatalf("seed %d %s: row %v, want final %v", seed, policy, got, rc.Final)
				}
			}
		}
	}
}

// SKIP never produces an action for a conflicting row; ACCEPT always leaves
// the branch's changed cells on main.
func TestPropertySkipAndAccept(t *testing.T) {
	for seed := uint64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 2))
		rows := randomChanges(rng, 20)
		ours := map[string]R{}
		for _, rc := range rows {
			if rng.IntN(4) > 0 { // main has some row, unrelated to base
				ours[OursKey(rc.Schema, rc.Table, rc.PK)] = randomRow(rng, 0).Project([]string{"a", "b", "c"})
				ours[OursKey(rc.Schema, rc.Table, rc.PK)]["id"] = rc.PK["id"]
			}
		}
		skip := Compute(Input{Rows: rows, Ours: ours, Policy: PolicySkip})
		conflicted := map[string]bool{}
		for _, c := range skip.Conflicts {
			conflicted[OursKey(c.Schema, c.Table, c.PK)] = true
		}
		for _, a := range skip.Actions {
			if conflicted[OursKey(a.Schema, a.Table, a.PK)] {
				t.Fatalf("seed %d: SKIP produced an action for conflicting row %v", seed, a.PK)
			}
		}
		if skip.Stats.RowsTotal != skip.Stats.Inserted+skip.Stats.Updated+skip.Stats.Deleted+skip.Stats.Skipped {
			t.Fatalf("seed %d: stats do not add up: %+v", seed, skip.Stats)
		}

		acc := Compute(Input{Rows: rows, Ours: ours, Policy: PolicyAccept})
		m := map[string]R{}
		for k, v := range ours {
			m[k] = v
		}
		applyModel(t, m, acc)
		for _, rc := range rows {
			got := m[OursKey(rc.Schema, rc.Table, rc.PK)]
			if rc.Final == nil {
				if got != nil {
					t.Fatalf("seed %d: ACCEPT left deleted row %v", seed, rc.PK)
				}
				continue
			}
			for _, c := range changedColumns(rc.Base, rc.Final) {
				if !changeset.ValueEqual(got[c], rc.Final[c]) {
					t.Fatalf("seed %d: ACCEPT row %v column %s = %v, want %v", seed, rc.PK, c, got[c], rc.Final[c])
				}
			}
		}
	}
}

func TestPlanCanonicalIsStable(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	rows := randomChanges(rng, 50)
	ours := map[string]R{}
	for _, rc := range rows {
		if rc.Base != nil {
			ours[OursKey(rc.Schema, rc.Table, rc.PK)] = rc.Base
		}
	}
	a, err := CanonicalPlan(Compute(Input{Rows: rows, Ours: ours, Policy: PolicySkip}), "demo", "ab")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalPlan(Compute(Input{Rows: rows, Ours: ours, Policy: PolicySkip}), "demo", "ab")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("plan JSON is not byte-identical across runs")
	}
}
