// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"sort"

	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// TableOrder returns the tables in topological order of their foreign keys:
// referenced tables before referencing ones. Ties, and the members of an FK
// cycle (including self-references), are ordered by (schema, name).
func TableOrder(tables []schema.Ref, edges []schema.FKEdge) []schema.Ref {
	sorted := make([]schema.Ref, len(tables))
	copy(sorted, tables)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })

	parents := map[schema.Ref]map[schema.Ref]bool{} // child -> parents not yet placed
	for _, e := range edges {
		if e.Child == e.Parent {
			continue
		}
		if parents[e.Child] == nil {
			parents[e.Child] = map[schema.Ref]bool{}
		}
		parents[e.Child][e.Parent] = true
	}
	placed := map[schema.Ref]bool{}
	var out []schema.Ref
	for len(out) < len(sorted) {
		progress := false
		for _, t := range sorted {
			if placed[t] {
				continue
			}
			ready := true
			for p := range parents[t] {
				if !placed[p] {
					ready = false
					break
				}
			}
			if ready {
				placed[t] = true
				out = append(out, t)
				progress = true
				break // restart so that ties keep (schema, name) order
			}
		}
		if !progress {
			// Cycle: place the first remaining table by name.
			for _, t := range sorted {
				if !placed[t] {
					placed[t] = true
					out = append(out, t)
					break
				}
			}
		}
	}
	return out
}

// OrderActions orders a plan's actions for execution (spec §10 step 8.3):
// phase 1 inserts and updates, tables in topological order, rows by
// first_seq; phase 2 deletes, tables in reverse topological order, rows by
// last_seq (the branch's own delete order).
func OrderActions(actions []merge.Action, tableOrder []schema.Ref) []merge.Action {
	rank := map[schema.Ref]int{}
	for i, t := range tableOrder {
		rank[t] = i
	}
	refOf := func(a merge.Action) schema.Ref { return schema.Ref{Schema: a.Schema, Name: a.Table} }
	var upserts, deletes []merge.Action
	for _, a := range actions {
		if a.Kind == merge.Delete {
			deletes = append(deletes, a)
		} else {
			upserts = append(upserts, a)
		}
	}
	sort.SliceStable(upserts, func(i, j int) bool {
		ri, rj := rank[refOf(upserts[i])], rank[refOf(upserts[j])]
		if ri != rj {
			return ri < rj
		}
		return upserts[i].FirstSeq < upserts[j].FirstSeq
	})
	sort.SliceStable(deletes, func(i, j int) bool {
		ri, rj := rank[refOf(deletes[i])], rank[refOf(deletes[j])]
		if ri != rj {
			return ri > rj
		}
		return deletes[i].LastSeq < deletes[j].LastSeq
	})
	return append(upserts, deletes...)
}
