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

// Package drain is the core (algorithm A): node-anchored, incremental,
// gang-aware greedy. A single dynamic pass repeatedly re-evaluates every
// still-freeable unit against the committed moves so far and commits the feasible
// one whose prospective plan is least disruptive — because gang damage is scored over
// the whole plan, a unit that reuses an already-broken gang is cheap, so the
// dynamic re-pick naturally prefers it (that's the "incremental gang-aware"
// part). Vacating a unit is atomic (all member nodes must empty via the
// feasibility solver, INV-RESCHED) and bounded by the disruption budget. The loop
// runs until no unit can be freed, then the plan is kept iff it meets MinNodesFreed.
//
// "Unit" generalizes "node": a node-domain plugin yields one single-node unit per
// node; a hypernode-domain plugin yields multi-node units. With both enabled
// the units are a weighted union and the core prefers higher-benefit units first.
package drain

import (
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

func init() {
	framework.RegisterCore(framework.CoreDrain, func() framework.Core { return &drainCore{} })
}

type drainCore struct{}

func (*drainCore) Name() string { return framework.CoreDrain }

// Plan runs the incremental, gang-aware drain over the session's freeable units.
func (*drainCore) Plan(ssn *framework.Session) (*api.RepackPlan, bool) {
	res := ssn.Resource()
	nodes := ssn.Nodes()
	movable := ssn.Movable()
	units := ssn.FreeableUnits()
	if len(units) == 0 || len(nodes) == 0 {
		return nil, false
	}
	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodesByName[n.Name] = n
		}
	}

	before := api.MeasureResource(nodes, res)
	klog.V(4).InfoS("repack drain: starting pass", "resource", res,
		"nodes", len(nodes), "freeableUnits", len(units),
		"occupiedNodes", before.B, "optimalNodes", before.A, "providingNodes", before.M)

	plan := drainGreedy(nodes, nodesByName, units, ssn, movable, res)
	if plan == nil {
		klog.V(4).InfoS("repack drain: no plan — nothing could be freed", "resource", res)
		return nil, false
	}
	plan.Before = before
	// Hard admissibility gate: the built-in benefit constraints (MinNodesFreed,
	// MinFragImprovementPercent) plus any plugin-registered plan constraints (e.g.
	// disruptionPolicy.maxDisruptionScore, added later). A rejected plan = NoRepackNeeded.
	if !ssn.PlanAdmissible(plan) {
		klog.V(3).InfoS("repack drain: plan rejected by benefit gate (below MinNodesFreed / MinFragImprovement)",
			"resource", res, "freedNodes", len(plan.FreedNodes), "moves", len(plan.Moves))
		return nil, false
	}
	plan.Cost = api.CostOf(plan.Moves, res)
	klog.V(3).InfoS("repack drain: plan accepted", "resource", res,
		"freedNodes", plan.FreedNodes, "moves", len(plan.Moves))
	return plan, true
}

// candidate is one unit that can be vacated this step, with the moves that vacate
// it and the disruption-budget deltas those moves imply.
type candidate struct {
	unit         api.FreeableUnit
	placed       []*api.Move
	newPodGroups map[schedapi.JobID]bool
	newCards     int64
	key          string
}

// drainState is the running state of one drain pass. Feasibility (which victims
// can relocate where) is delegated to the snapshot's scheduler-faithful feasibility check
// (Snapshot.FeasibleRelocation); this struct only tracks the greedy pass progress
// and the disruption budget.
type drainState struct {
	// Fixed for the whole pass.
	ssn          *framework.Session
	snapshot     framework.Snapshot
	nodes        []*schedapi.NodeInfo
	nodesByName  map[string]*schedapi.NodeInfo
	movable      api.Movable
	resource     v1.ResourceName
	maxPodGroups int
	maxResource  int64

	// Mutated as units are committed.
	drained        map[string]bool // emptied — no longer a receiver
	filled         map[string]bool // received a moved-in pod
	provenStuck    map[string]bool // proven un-vacatable this pass → prefer as receiver
	movedPodGroups map[schedapi.JobID]bool
	movedCards     int64
	moves          []*api.Move
	freedNodes     []string
	freedUnits     []api.FreeableUnit
}

// drainGreedy is the single dynamic pass. Each step re-evaluates every still-
// freeable unit and commits the feasible one whose prospective plan is least
// disruptive. Terminates because each commit drains >= 1 node, and a drained node
// never becomes freeable again.
func drainGreedy(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	units []api.FreeableUnit,
	ssn *framework.Session,
	movable api.Movable,
	res v1.ResourceName,
) *api.RepackPlan {
	s := newDrainState(nodes, nodesByName, ssn, movable, res)
	for step := 1; ; step++ {
		// 1. Evaluate every still-freeable unit against the committed moves so far.
		var feasible []candidate
		for _, unit := range units {
			if c, ok := s.evaluateUnit(unit); ok {
				feasible = append(feasible, c)
			}
		}
		klog.V(4).InfoS("repack drain: step evaluated units", "step", step,
			"totalUnits", len(units), "feasibleThisStep", len(feasible), "nodesFreedSoFar", len(s.freedNodes))
		if len(feasible) == 0 {
			break
		}
		// 2. Pick the least-disruptive one and 3. commit it.
		chosen := s.chooseLeastDisruptive(feasible)
		klog.V(4).InfoS("repack drain: committing unit", "step", step, "unit", chosen.key,
			"freesNodes", chosen.unit.Nodes, "moves", len(chosen.placed), "cards", chosen.newCards)
		s.commit(chosen)
	}
	// 4. A pass that freed nothing yields no plan.
	return s.plan()
}

// newDrainState caches the per-pass budget caps and initializes progress maps.
func newDrainState(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	ssn *framework.Session,
	movable api.Movable,
	res v1.ResourceName,
) *drainState {
	return &drainState{
		ssn:            ssn,
		snapshot:       ssn.Snapshot(),
		nodes:          nodes,
		nodesByName:    nodesByName,
		movable:        movable,
		resource:       res,
		maxPodGroups:   ssn.MaxPodGroups(),
		maxResource:    ssn.MaxResource(),
		drained:        make(map[string]bool),
		filled:         make(map[string]bool),
		provenStuck:    make(map[string]bool),
		movedPodGroups: make(map[schedapi.JobID]bool),
	}
}

// evaluateUnit checks whether `unit` can be vacated now — every victim relocates
// onto a surviving receiver within the disruption budget — and returns the moves
// that vacate it. ok=false means "not freeable this step".
func (s *drainState) evaluateUnit(unit api.FreeableUnit) (candidate, bool) {
	key := unitKey(unit)
	inUnit, ok := freeableNow(unit, s.nodesByName, s.drained, s.filled, s.movable, s.resource)
	if !ok {
		klog.V(5).InfoS("repack drain: unit not freeable now (drained/filled/has-immovable-pod)", "unit", key, "nodes", unit.Nodes)
		return candidate{}, false
	}
	// Skip accelerator-empty units: freeing a node that runs no accelerator pod
	// isn't defrag (its accelerator capacity is already idle).
	accelerated := false
	for _, nodeName := range unit.Nodes {
		if occupiesAccelerator(s.nodesByName[nodeName], s.resource) {
			accelerated = true
			break
		}
	}
	if !accelerated {
		klog.V(5).InfoS("repack drain: skip unit — no accelerator pods on it", "unit", key, "nodes", unit.Nodes)
		return candidate{}, false
	}
	var victims []*schedapi.TaskInfo
	for _, nodeName := range unit.Nodes {
		victims = append(victims, api.VictimsOf(s.nodesByName[nodeName], s.movable, s.resource)...)
	}
	if len(victims) == 0 {
		klog.V(5).InfoS("repack drain: skip unit — no movable accelerator victims", "unit", key, "nodes", unit.Nodes)
		return candidate{}, false
	}
	receivers := s.receiversInPreferenceOrder(inUnit)
	klog.V(5).InfoS("repack drain: evaluating unit feasibility", "unit", key,
		"victims", taskNames(victims), "victimCount", len(victims),
		"receivers", nodeNames(receivers), "receiverCount", len(receivers))
	// Feasibility = the scheduler-faithful relocation feasibility check: it simulates evicting
	// these victims and greedily placing them onto the receivers (in the preference
	// order we pass) with the full scheduler filter stack, over the moves already
	// committed this pass.
	placed, feasible := s.snapshot.FeasibleRelocation(s.moves, victims, receivers)
	if !feasible {
		// Vacatability is monotonic (slack only shrinks), so a unit infeasible now
		// stays infeasible — remember its nodes as preferred (staying) receivers.
		klog.V(4).InfoS("repack drain: unit INFEASIBLE — victims cannot all relocate onto receivers",
			"unit", key, "victims", len(victims), "receivers", len(receivers))
		for _, nodeName := range unit.Nodes {
			s.provenStuck[nodeName] = true
		}
		return candidate{}, false
	}
	// Disruption budget (maxPerRun): prospective deltas.
	newPodGroups := make(map[schedapi.JobID]bool)
	var newCards int64
	for _, v := range victims {
		if !s.movedPodGroups[v.Job] {
			newPodGroups[v.Job] = true
		}
		newCards += api.Scalar(v.InitResreq, s.resource)
	}
	if s.maxPodGroups > 0 && len(s.movedPodGroups)+len(newPodGroups) > s.maxPodGroups {
		klog.V(5).InfoS("repack drain: skip unit — would exceed maxPerRun.podGroups", "unit", key,
			"wouldBe", len(s.movedPodGroups)+len(newPodGroups), "max", s.maxPodGroups)
		return candidate{}, false
	}
	if s.maxResource > 0 && s.movedCards+newCards > s.maxResource {
		klog.V(5).InfoS("repack drain: skip unit — would exceed maxPerRun.resources", "unit", key,
			"wouldBe", s.movedCards+newCards, "max", s.maxResource)
		return candidate{}, false
	}
	if klog.V(5).Enabled() {
		for _, m := range placed {
			klog.V(5).InfoS("repack drain: victim placement", "unit", key,
				"pod", m.Task.Name, "from", m.From, "to", m.To)
		}
	}
	klog.V(5).InfoS("repack drain: unit FEASIBLE", "unit", key, "moves", len(placed), "cards", newCards)
	return candidate{unit: unit, placed: placed, newPodGroups: newPodGroups, newCards: newCards, key: key}, true
}

// receiversInPreferenceOrder returns the nodes that may receive relocated pods, in
// the order the drain prefers to fill them. A node is eligible if it is not in the
// unit being vacated, not already drained, and not accelerator-EMPTY (0 pods
// requesting the accelerator — a CPU/memory-only node counts as empty too):
// draining onto one just relights a free accelerator node (net-zero shuffle).
//
// Order: STAYING nodes first (they remain occupied regardless, so filling their
// slack never wastes a drainable node's empty-ability), then best-fit (tightest
// target-resource FutureIdle) within a tier so relocations consolidate onto
// already-loaded nodes rather than lighting up near-empty ones (§4.9).
func (s *drainState) receiversInPreferenceOrder(inUnit map[string]bool) []*schedapi.NodeInfo {
	receivers := make([]*schedapi.NodeInfo, 0, len(s.nodes))
	for _, n := range s.nodes {
		if inUnit[n.Name] || s.drained[n.Name] || !occupiesAccelerator(n, s.resource) {
			continue
		}
		receivers = append(receivers, n)
	}
	sort.SliceStable(receivers, func(i, j int) bool {
		si, sj := s.staying(receivers[i]), s.staying(receivers[j])
		if si != sj {
			return si // staying nodes first
		}
		return receiverSlack(receivers[i], s.resource) < receiverSlack(receivers[j], s.resource)
	})
	return receivers
}

// receiverSlack is the target-resource free capacity used to best-fit sort receivers.
// Prefer FutureIdle (scheduler cache); fall back to Allocatable−Used for test nodes
// that only set Used/Allocatable without initializing Idle.
func receiverSlack(n *schedapi.NodeInfo, res v1.ResourceName) int64 {
	if n == nil {
		return 0
	}
	if n.Idle != nil {
		return api.Scalar(n.FutureIdle(), res)
	}
	if n.Allocatable == nil {
		return 0
	}
	free := n.Allocatable.Clone()
	if n.Used != nil {
		free.SubWithoutAssert(n.Used)
	}
	return api.Scalar(free, res)
}

// staying reports whether a node will remain occupied regardless of this pass — so
// it is a preferred receiver. A node stays if it has an immovable accelerator pod
// (not freeable), is receiver-only (excluded from draining by scope.nodes), or was
// already proven un-vacatable this pass.
func (s *drainState) staying(n *schedapi.NodeInfo) bool {
	return !api.NodeFreeable(n, s.movable, s.resource) || !s.snapshot.NodeInScope(n) || s.provenStuck[n.Name]
}

// chooseLeastDisruptive orders the feasible candidates deterministically (higher
// benefit first, then by unit key) and returns the least-disruptive one over the
// whole prospective plan (already-committed moves + the candidate's).
func (s *drainState) chooseLeastDisruptive(feasible []candidate) candidate {
	sort.SliceStable(feasible, func(i, j int) bool {
		if feasible[i].unit.Weight != feasible[j].unit.Weight {
			return feasible[i].unit.Weight > feasible[j].unit.Weight
		}
		return feasible[i].key < feasible[j].key
	})
	candidatePlans := make([]*api.CandidatePlan, len(feasible))
	for i, c := range feasible {
		combinedMoves := make([]*api.Move, 0, len(s.moves)+len(c.placed))
		combinedMoves = append(combinedMoves, s.moves...)
		combinedMoves = append(combinedMoves, c.placed...)
		candidatePlans[i] = &api.CandidatePlan{Moves: combinedMoves}
	}
	return feasible[s.ssn.LeastDisruptive(candidatePlans)]
}

// commit applies the chosen candidate to the pass state: mark receivers filled and
// the unit's nodes drained, record moved PodGroups/cards, and append the moves.
func (s *drainState) commit(chosen candidate) {
	for _, m := range chosen.placed {
		s.filled[m.To] = true
	}
	for pg := range chosen.newPodGroups {
		s.movedPodGroups[pg] = true
	}
	s.movedCards += chosen.newCards
	for _, nodeName := range chosen.unit.Nodes {
		s.drained[nodeName] = true
		s.freedNodes = append(s.freedNodes, nodeName)
	}
	s.freedUnits = append(s.freedUnits, chosen.unit)
	s.moves = append(s.moves, chosen.placed...)
}

// plan is the pass result: nil when nothing was freed.
func (s *drainState) plan() *api.RepackPlan {
	if len(s.freedNodes) == 0 {
		return nil
	}
	return &api.RepackPlan{Moves: s.moves, FreedNodes: s.freedNodes, FreedUnits: s.freedUnits}
}

// freeableNow reports whether every node of the unit can still be a drain target
// (present, not already drained, not a receiver/filled, and freeable), returning
// the unit's node set.
func freeableNow(unit api.FreeableUnit, nodesByName map[string]*schedapi.NodeInfo, drained, filled map[string]bool, movable api.Movable, res v1.ResourceName) (map[string]bool, bool) {
	inUnit := make(map[string]bool, len(unit.Nodes))
	for _, nodeName := range unit.Nodes {
		n := nodesByName[nodeName]
		if n == nil || drained[nodeName] || filled[nodeName] || !api.NodeFreeable(n, movable, res) {
			return nil, false
		}
		inUnit[nodeName] = true
	}
	return inUnit, len(inUnit) > 0
}

// occupiesAccelerator reports whether the node uses any of the target accelerator
// resource — the SAME criterion MeasureResource uses to count a node as occupied
// (B), so "the drain treats this node as empty" ⟺ "the fragmentation metric does
// not count it". A node with only CPU/memory pods is empty for defrag: its
// accelerator capacity is idle, so filling it just lights up a fresh accelerator
// node (net-zero), and freeing it is not a consolidation. Everything (empty/full/
// fragmentation) is judged by the resource being defragmented (goals[0].resource).
func occupiesAccelerator(n *schedapi.NodeInfo, res v1.ResourceName) bool {
	return n != nil && n.Used != nil && api.Scalar(n.Used, res) > 0
}

// unitKey is a deterministic identifier for a unit (sorted member node names).
// taskNames and nodeNames render slices as plain name lists for readable logs.
func taskNames(tasks []*schedapi.TaskInfo) []string {
	names := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t != nil {
			names = append(names, t.Name)
		}
	}
	return names
}

func nodeNames(nodes []*schedapi.NodeInfo) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			names = append(names, n.Name)
		}
	}
	return names
}

func unitKey(u api.FreeableUnit) string {
	ns := append([]string(nil), u.Nodes...)
	sort.Strings(ns)
	return strings.Join(ns, ",")
}
