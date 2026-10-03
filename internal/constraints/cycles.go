package constraints

import (
	"slices"
	"sort"
)

// ruleCycles returns every nontrivial strongly connected component of the
// group graph induced on the rule's groups, ordered by members.
//
// This is a fresh Tarjan pass rather than store.DetectCycles: that function is
// file-level, applies no trust filter and enumerates back edges, so it can say
// whether a cycle exists but neither count nor group components. Here every
// component is found regardless of paging, so summary.cycles is exact.
func ruleCycles(r Rule, ordered []*agg) []Cycle {
	inRule := map[string]bool{}
	for _, g := range r.Groups {
		inRule[g] = true
	}
	// witness[from][to] is the first dependency by identity order; ordered is
	// already sorted, so the first one seen wins.
	witness := map[string]map[string]*agg{}
	for _, a := range ordered {
		if !inRule[a.src] || !inRule[a.dst] {
			continue
		}
		if witness[a.src] == nil {
			witness[a.src] = map[string]*agg{}
		}
		if witness[a.src][a.dst] == nil {
			witness[a.src][a.dst] = a
		}
	}
	adj := map[string][]string{}
	for from, tos := range witness {
		for to := range tos {
			adj[from] = append(adj[from], to)
		}
		sort.Strings(adj[from])
	}

	var cycles []Cycle
	for _, members := range tarjan(r.Groups, adj) {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		path := shortestReturn(members, adj)
		c := Cycle{RuleID: r.ID, Members: members, Witness: path, Hops: []Hop{}}
		for i := 0; i+1 < len(path); i++ {
			c.Hops = append(c.Hops, Hop{From: path[i], To: path[i+1], Dependency: witness[path[i]][path[i+1]].dependency()})
		}
		cycles = append(cycles, c)
	}
	sort.Slice(cycles, func(i, j int) bool { return slices.Compare(cycles[i].Members, cycles[j].Members) < 0 })
	return cycles
}

// tarjan returns the strongly connected components of the graph. nodes and
// every adjacency list are sorted, so the traversal is deterministic.
func tarjan(nodes []string, adj map[string][]string) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	next := 0
	var visit func(v string)
	visit = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			out = append(out, comp)
		}
	}
	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			visit(v)
		}
	}
	return out
}

// shortestReturn is the lexicographically smallest group sequence among the
// shortest paths from the smallest member back to itself, inside the component.
//
// BFS over sorted adjacency gives every node its lexicographically smallest
// shortest path from the start: within a layer the queue is in path order, and
// a node keeps its first (smallest) discoverer as parent.
func shortestReturn(members []string, adj map[string][]string) []string {
	start := members[0]
	inComp := map[string]bool{}
	for _, m := range members {
		inComp[m] = true
	}
	parent := map[string]string{}
	dist := map[string]int{start: 0}
	queue := []string{start}
	var best []string
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if best != nil && dist[v]+1 > len(best)-1 {
			break
		}
		for _, w := range adj[v] {
			if !inComp[w] {
				continue
			}
			if w == start {
				var p []string
				for x := v; ; x = parent[x] {
					p = append(p, x)
					if x == start {
						break
					}
				}
				slices.Reverse(p)
				p = append(p, start)
				if best == nil || slices.Compare(p, best) < 0 {
					best = p
				}
				continue
			}
			if _, seen := dist[w]; !seen {
				dist[w] = dist[v] + 1
				parent[w] = v
				queue = append(queue, w)
			}
		}
	}
	return best
}
