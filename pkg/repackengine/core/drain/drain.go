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

// Package drain is the P0 core (algorithm A): node-anchored, incremental,
// gang-aware greedy. A single dynamic pass repeatedly re-evaluates every
// still-freeable unit against the current ledger and commits the feasible one
// whose prospective plan is least disruptive — because gang damage is scored over
// the whole plan, a unit that reuses an already-broken gang is cheap, so the
// dynamic re-pick naturally prefers it (that's the "incremental gang-aware"
// part). Vacating a unit is atomic (all member nodes must empty via the
// feasibility solver, INV-RESCHED) and bounded by the disruption budget. The loop
// runs until no unit can be freed, then the plan is kept iff it meets MinNodesFreed.
//
// "Unit" generalizes "node": a node-domain plugin yields one single-node unit per
// node (P0); a hypernode-domain plugin yields multi-node units. With both enabled
// the units are a weighted union and the core prefers higher-benefit units first.
package drain

import (
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

func init() {
	framework.RegisterCore(framework.CoreDrain, func() framework.Core { return &drainCore{} })
}

type drainCore struct{}

func (*drainCore) Name() string { return framework.CoreDrain }

// Receiver-preference tiers for the drain (higher = filled first). A node that
// will definitely stay occupied is the best receiver — filling its slack never
// wastes a drainable node's empty-ability; a still-drainable node is filled only
// when necessary, so its own empty-ability is preserved.
const (
	preferDrainable = 1
	preferStaying   = 2
)

// Plan runs the incremental, gang-aware drain over the session's freeable units.
func (*drainCore) Plan(ssn *framework.Session) (*api.RepackPlan, bool) {
	res := ssn.Resource()
	nodes := ssn.Nodes()
	movable := ssn.Movable()
	free := ssn.Free()
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

	plan := drainGreedy(nodes, nodesByName, units, ssn, movable, free, res)
	if plan == nil {
		return nil, false
	}
	plan.Before = api.MeasureResource(nodes, res)
	// Hard admissibility gate: the built-in benefit constraints (MinNodesFreed,
	// MinFragImprovementPercent) plus any plugin-registered plan constraints (e.g.
	// disruptionPolicy.maxDisruptionScore in P1). A rejected plan = NoRepackNeeded.
	if !ssn.PlanAdmissible(plan) {
		return nil, false
	}
	plan.Cost = api.CostOf(plan.Moves, res)
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

// drainState is the running state of one drain pass. It also implements
// api.ReceiverPolicy (Free/Fit/Prefer) so it can be handed straight to the
// feasibility solver — the receiver rules read the same live ledger and progress
// the pass mutates, with no closures threaded around.
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
	ledger         map[string]*schedapi.Resource // node -> remaining free capacity
	drained        map[string]bool               // emptied — no longer a receiver
	filled         map[string]bool               // received a moved-in pod
	provenStuck    map[string]bool               // proven un-vacatable → preferred receiver
	movedPodGroups map[schedapi.JobID]bool
	movedCards     int64
	moves          []*api.Move
	freedNodes     []string
	freedUnits     []api.FreeableUnit
}

var _ api.ReceiverPolicy = (*drainState)(nil)

// Free is a receiver's remaining capacity from the running ledger.
func (s *drainState) Free(n *schedapi.NodeInfo) *schedapi.Resource { return s.ledger[n.Name] }

// Fit tests whether a victim can reschedule onto a receiver (affinity/taint/
// topology/device), treating the victim as unbound.
func (s *drainState) Fit(t *schedapi.TaskInfo, n *schedapi.NodeInfo) bool {
	return s.ssn.PredicateForReschedule(t, n) == nil
}

// Prefer: nodes that will definitely stay occupied are the best receivers —
// filling their slack never wastes a drainable node. Staying = has an immovable
// pod (e.g. an excluded PodGroup, so the node can never be vacated), out of
// scope.nodes (user excluded it from draining — receiver only), or already proven
// un-vacatable this pass.
func (s *drainState) Prefer(n *schedapi.NodeInfo) int {
	if !api.NodeFreeable(n, s.movable, s.resource) || s.provenStuck[n.Name] || !s.snapshot.NodeInScope(n) {
		return preferStaying
	}
	return preferDrainable
}

// drainGreedy is the single dynamic pass. Each step re-evaluates every still-
// freeable unit against the current ledger and commits the feasible one whose
// prospective plan is least disruptive. Terminates because each commit drains
// >= 1 node, and a drained node never becomes freeable again.
func drainGreedy(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	units []api.FreeableUnit,
	ssn *framework.Session,
	movable api.Movable,
	free func(*schedapi.NodeInfo) *schedapi.Resource,
	res v1.ResourceName,
) *api.RepackPlan {
	s := newDrainState(nodes, nodesByName, ssn, movable, free, res)
	for {
		// 1. Evaluate every still-freeable unit against the current ledger.
		var feasible []candidate
		for _, unit := range units {
			if c, ok := s.evaluateUnit(unit); ok {
				feasible = append(feasible, c)
			}
		}
		if len(feasible) == 0 {
			break
		}
		// 2. Pick the least-disruptive one and 3. commit it.
		s.commit(s.chooseLeastDisruptive(feasible))
	}
	// 4. A pass that freed nothing yields no plan.
	return s.plan()
}

// newDrainState seeds the ledger from each node's free-capacity basis (cloned so
// the caller's NodeInfo is never mutated) and caches the per-pass budget caps.
func newDrainState(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	ssn *framework.Session,
	movable api.Movable,
	free func(*schedapi.NodeInfo) *schedapi.Resource,
	res v1.ResourceName,
) *drainState {
	ledger := make(map[string]*schedapi.Resource, len(nodes))
	for _, n := range nodes {
		f := free(n)
		if f == nil {
			f = schedapi.EmptyResource()
		}
		ledger[n.Name] = f.Clone()
	}
	return &drainState{
		ssn:            ssn,
		snapshot:       ssn.Snapshot(),
		nodes:          nodes,
		nodesByName:    nodesByName,
		movable:        movable,
		resource:       res,
		maxPodGroups:   ssn.MaxPodGroups(),
		maxResource:    ssn.MaxResource(),
		ledger:         ledger,
		drained:        make(map[string]bool),
		filled:         make(map[string]bool),
		provenStuck:    make(map[string]bool),
		movedPodGroups: make(map[schedapi.JobID]bool),
	}
}

// evaluateUnit checks whether `unit` can be vacated now — every victim reschedules
// onto a surviving receiver within the disruption budget — and returns the moves
// that vacate it. ok=false means "not freeable this step".
func (s *drainState) evaluateUnit(unit api.FreeableUnit) (candidate, bool) {
	inUnit, ok := freeableNow(unit, s.nodesByName, s.drained, s.filled, s.movable, s.resource)
	if !ok {
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
		return candidate{}, false
	}
	var victims []*schedapi.TaskInfo
	for _, nodeName := range unit.Nodes {
		victims = append(victims, api.VictimsOf(s.nodesByName[nodeName], s.movable, s.resource)...)
	}
	if len(victims) == 0 {
		return candidate{}, false
	}
	placed, feasible := api.NewDomain(s.receiversExcluding(inUnit), s).Feasible(victims)
	if !feasible {
		// Vacatability is monotonic (slack only shrinks), so a unit infeasible now
		// stays infeasible — cache its nodes as preferred receivers.
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
		return candidate{}, false
	}
	if s.maxResource > 0 && s.movedCards+newCards > s.maxResource {
		return candidate{}, false
	}
	return candidate{unit: unit, placed: placed, newPodGroups: newPodGroups, newCards: newCards, key: unitKey(unit)}, true
}

// receiversExcluding returns the nodes that may receive relocated pods: not in the
// unit being vacated, not already drained, and not accelerator-EMPTY (0 pods
// requesting the accelerator — a CPU/memory-only node counts as empty too):
// draining onto one just relights a free accelerator node (net-zero shuffle). Full
// nodes (no slack) are filtered by the solver.
func (s *drainState) receiversExcluding(inUnit map[string]bool) []*schedapi.NodeInfo {
	receivers := make([]*schedapi.NodeInfo, 0, len(s.nodes))
	for _, n := range s.nodes {
		if inUnit[n.Name] || s.drained[n.Name] || !occupiesAccelerator(n, s.resource) {
			continue
		}
		receivers = append(receivers, n)
	}
	return receivers
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

// commit applies the chosen candidate to the pass state: debit receiver ledgers,
// record moved PodGroups/cards, mark the unit's nodes drained, and append moves.
func (s *drainState) commit(chosen candidate) {
	for _, m := range chosen.placed {
		if r := s.ledger[m.To]; r != nil {
			r.Sub(m.Task.InitResreq)
		}
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
func unitKey(u api.FreeableUnit) string {
	ns := append([]string(nil), u.Nodes...)
	sort.Strings(ns)
	return strings.Join(ns, ",")
}
