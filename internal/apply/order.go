// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"sort"

	"github.com/keeptalking83/pgweir/internal/merge"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// TableOrder returns the tables in topological order of their foreign keys:
// referenced tables before referencing ones. Tables in a foreign-key cycle
// (including self-references) form one strongly connected component, which
// is placed as a unit once every table it references outside the cycle is
// placed; inside a component, and between components that are ready at the
// same time, tables are ordered by (schema, name).
func TableOrder(tables []schema.Ref, edges []schema.FKEdge) []schema.Ref {
	sorted := make([]schema.Ref, len(tables))
	copy(sorted, tables)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })
	index := map[schema.Ref]int{}
	for i, t := range sorted {
		index[t] = i
	}
	parents := make([][]int, len(sorted)) // child -> referenced tables
	for _, e := range edges {
		c, okC := index[e.Child]
		p, okP := index[e.Parent]
		if okC && okP {
			parents[c] = append(parents[c], p)
		}
	}
	comp := components(len(sorted), parents)

	// Component DAG: a component depends on the components it references.
	n := 0
	for _, c := range comp {
		n = max(n, c+1)
	}
	deps := make([]map[int]bool, n)
	members := make([][]int, n)
	for v := range sorted {
		members[comp[v]] = append(members[comp[v]], v) // ascending, so sorted by name
		for _, p := range parents[v] {
			if comp[p] != comp[v] {
				if deps[comp[v]] == nil {
					deps[comp[v]] = map[int]bool{}
				}
				deps[comp[v]][comp[p]] = true
			}
		}
	}
	placed := make([]bool, n)
	var out []schema.Ref
	for len(out) < len(sorted) {
		best := -1
		for c := 0; c < n; c++ {
			if placed[c] {
				continue
			}
			ready := true
			for d := range deps[c] {
				if !placed[d] {
					ready = false
					break
				}
			}
			if ready && (best < 0 || members[c][0] < members[best][0]) {
				best = c
			}
		}
		placed[best] = true
		for _, v := range members[best] {
			out = append(out, sorted[v])
		}
	}
	return out
}

// components labels the strongly connected components of a graph (Tarjan).
func components(n int, adj [][]int) []int {
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	comp := make([]int, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int
	next, count := 0, 0
	var visit func(v int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if index[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp[w] = count
				if w == v {
					break
				}
			}
			count++
		}
	}
	for v := 0; v < n; v++ {
		if index[v] < 0 {
			visit(v)
		}
	}
	return comp
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
