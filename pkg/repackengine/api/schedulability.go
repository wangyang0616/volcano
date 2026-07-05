/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Schedulability oracle for the repack (defragmentation) engine.
//
// Repack is a *rearrangement*, not a preemption: every pod it evicts must be
// re-schedulable somewhere afterwards (design invariant INV-RESCHED, §4.9). A
// candidate plan that frees nodes but leaves an evicted pod homeless is
// rejected before anything is acted on.
//
// This file holds the pure feasibility solver: given the tasks that must find
// a home and a domain of destination nodes (each with a working free-resource
// ledger), decide whether a complete assignment exists. It depends only on the
// resource model and a pluggable Fit predicate, so it is exercised in unit
// tests with fakes and cross-checked against a brute-force assignment search.
// The live-Session wiring (Statement.Evict/Pipeline, ssn.PredicateFn) lives in
// schedulability_engine.go.
package api

import (
	"sort"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// Fit reports whether task may run on node, judging everything *except* the
// running resource ledger the solver tracks itself: node selectors, affinity /
// anti-affinity, taints / tolerations, topology and device constraints. The
// engine adapter implements it with ssn.PredicateFn; tests supply a fake.
// A nil Fit means "resource fit only".
type Fit func(task *api.TaskInfo, node *api.NodeInfo) bool

// Move is a decided (re)placement of one task produced by the solver.
type Move struct {
	Task *api.TaskInfo
	From string // current node, "" if the task is pending/unscheduled
	To   string // chosen destination node
}

// freeNode pairs a node with the mutable capacity the solver consumes as it
// places tasks during the search.
type freeNode struct {
	info *api.NodeInfo
	free *api.Resource
}

// Domain is the candidate destination set for a feasibility search. Construct
// it with NewDomain from the nodes that survive the repack (i.e. excluding the
// nodes the plan intends to free), seeding each node's capacity from a free
// function — typically NodeInfo.FutureIdle, which already credits resources
// released by victims evicted in the surrounding Statement.
type Domain struct {
	nodes  []*freeNode
	fit    Fit
	prefer func(*api.NodeInfo) int // higher = preferred receiver; nil = neutral
}

// NewDomain builds a search domain. free returns the working free capacity for
// a node; it is cloned so the caller's NodeInfo is never mutated.
func NewDomain(nodes []*api.NodeInfo, free func(*api.NodeInfo) *api.Resource, fit Fit) *Domain {
	d := &Domain{fit: fit}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		cap := free(n)
		if cap == nil {
			cap = api.EmptyResource()
		}
		d.nodes = append(d.nodes, &freeNode{info: n, free: cap.Clone()})
	}
	return d
}

// Prefer biases receiver selection: among the nodes that fit a task, those with a
// higher preference are filled first (e.g. nodes that will definitely stay
// occupied — so filling them never wastes a drainable node's empty-ability).
// Best-fit (tightest capacity) still orders within one preference tier. Preference
// changes only which feasible assignment is found first, never feasibility. nil
// (the default) is neutral. Returns d for chaining.
func (d *Domain) Prefer(fn func(*api.NodeInfo) int) *Domain {
	d.prefer = fn
	return d
}

// Feasible decides whether every task in place can be (re)scheduled onto the
// domain: each lands on a node whose working capacity covers its request and
// that satisfies Fit, with no node oversubscribed. It runs a first-fit-
// decreasing search with backtracking, so the search is complete for the
// modeled constraints — a false result means no assignment exists, which the
// caller treats as an INV-RESCHED violation and rejects the plan.
//
// On success the returned moves give each task's destination and the domain's
// ledger reflects the placements; on failure the ledger is left unchanged.
//
// Gang semantics: tasks are placed individually here. All-or-nothing for a
// relief PodGroup is enforced by the caller passing that PodGroup's tasks
// together and requiring every one of them to appear in moves.
func (d *Domain) Feasible(place []*api.TaskInfo) (moves []*Move, ok bool) {
	// FFD: place the largest requests first to fail fast and pack tightly.
	order := make([]*api.TaskInfo, 0, len(place))
	for _, t := range place {
		if t != nil {
			order = append(order, t)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return magnitude(order[i].InitResreq) > magnitude(order[j].InitResreq)
	})

	moves = make([]*Move, 0, len(order))
	if d.assign(order, 0, &moves) {
		return moves, true
	}
	return nil, false
}

// assign recursively places order[i:] via backtracking.
//
// Candidate nodes are tried by receiver preference (Prefer, higher first — e.g.
// nodes that will definitely stay occupied), then best-fit within a tier (tightest
// remaining capacity), so a displaced pod consolidates onto an already-loaded,
// staying node rather than lighting up a near-empty one — which would just move
// fragmentation around instead of removing it (design §4.9, "don't disturb
// well-packed nodes"). Ordering only changes which feasible solution is found
// first; the search is still exhaustive, so a false result remains a true "no
// assignment exists".
func (d *Domain) assign(order []*api.TaskInfo, i int, moves *[]*Move) bool {
	if i == len(order) {
		return true
	}
	task := order[i]
	req := task.InitResreq
	if req == nil {
		req = api.EmptyResource()
	}

	// Collect feasible nodes for this task, tightest-fit first.
	cand := make([]*freeNode, 0, len(d.nodes))
	for _, n := range d.nodes {
		if !req.LessEqual(n.free, api.Zero) {
			continue // not enough room on this node
		}
		if d.fit != nil && !d.fit(task, n.info) {
			continue // predicate (affinity/taint/topology/...) rejects it
		}
		cand = append(cand, n)
	}
	sort.SliceStable(cand, func(a, b int) bool {
		if d.prefer != nil {
			pa, pb := d.prefer(cand[a].info), d.prefer(cand[b].info)
			if pa != pb {
				return pa > pb // higher-preference receiver first
			}
		}
		return magnitude(cand[a].free) < magnitude(cand[b].free) // then least free = most loaded
	})

	for _, n := range cand {
		n.free.Sub(req) // guarded by LessEqual above, so never goes negative
		*moves = append(*moves, &Move{Task: task, From: task.NodeName, To: n.info.Name})

		if d.assign(order, i+1, moves) {
			return true
		}

		n.free.Add(req) // backtrack
		*moves = (*moves)[:len(*moves)-1]
	}
	return false
}

// magnitude is a rough size used only to order tasks for the FFD heuristic;
// accelerators dominate repack, so scalars are summed alongside cpu and memory.
func magnitude(r *api.Resource) float64 {
	if r == nil {
		return 0
	}
	m := r.MilliCPU + r.Memory
	for _, q := range r.ScalarResources {
		m += q
	}
	return m
}
