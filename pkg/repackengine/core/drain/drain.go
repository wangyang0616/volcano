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
	minFreed := ssn.MinNodesFreed()
	if minFreed < 1 {
		minFreed = 1
	}
	byName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, n := range nodes {
		if n != nil {
			byName[n.Name] = n
		}
	}

	plan := drainGreedy(nodes, byName, units, ssn, movable, free, res)
	if plan == nil {
		return nil, false
	}
	plan.Before = api.MeasureResource(nodes, res)
	if plan.Benefit() < float64(minFreed) {
		return nil, false // below the node-count benefit gate (NoRepackNeeded)
	}
	// Per-run benefit gate: the run's spec.goals[0].minFragImprovementPercent —
	// improvement (before-after) in percentage points must clear it. FragRateDelta
	// is negative (fragmentation dropped), so improvement% = round(-delta*100).
	if minImprove := ssn.MinFragImprovementPercent(); minImprove > 0 {
		improvePct := int(-plan.FragRateDelta()*100 + 0.5)
		if improvePct < minImprove {
			return nil, false // fragmentation improvement below the run's threshold
		}
	}
	plan.Cost = api.CostOf(plan.Moves, res)
	return plan, true
}

// drainGreedy is the single dynamic pass. Ledger/drained/filled/budget state
// carries across steps; each step re-evaluates every still-freeable unit and
// commits the feasible one whose prospective plan (committed moves + this unit's)
// is least disruptive. Terminates because each commit drains >= 1 node, and a
// drained node never becomes freeable again.
func drainGreedy(
	nodes []*schedapi.NodeInfo,
	byName map[string]*schedapi.NodeInfo,
	units []api.FreeableUnit,
	ssn *framework.Session,
	movable api.Movable,
	free func(*schedapi.NodeInfo) *schedapi.Resource,
	res v1.ResourceName,
) *api.RepackPlan {
	ledger := make(map[string]*schedapi.Resource, len(nodes))
	for _, n := range nodes {
		f := free(n)
		if f == nil {
			f = schedapi.EmptyResource()
		}
		ledger[n.Name] = f.Clone()
	}
	drained := make(map[string]bool)     // emptied — no longer a receiver
	filled := make(map[string]bool)      // received a moved-in pod
	provenStuck := make(map[string]bool) // proven un-vacatable → preferred receiver
	movedPGs := make(map[schedapi.JobID]bool)
	var movedCards int64
	var moves []*api.Move
	var freedNodes []string
	var freedUnits []api.FreeableUnit
	fit := func(t *schedapi.TaskInfo, n *schedapi.NodeInfo) bool { return ssn.Predicate(t, n) == nil }
	// prefer: nodes that will definitely stay occupied are the best receivers —
	// filling their slack never wastes a drainable node. Staying = has an
	// immovable pod (e.g. an excluded PodGroup, so the node can never be vacated),
	// out of scope.nodes (user excluded it from draining — receiver only), or
	// already proven un-vacatable this pass.
	snap := ssn.Snapshot()
	prefer := func(n *schedapi.NodeInfo) int {
		if !api.NodeFreeable(n, movable) || provenStuck[n.Name] || !snap.NodeInScope(n) {
			return 2
		}
		return 1
	}

	type cand struct {
		unit     api.FreeableUnit
		placed   []*api.Move
		newPGs   map[schedapi.JobID]bool
		newCards int64
		key      string
	}

	for {
		var feas []cand
		for _, unit := range units {
			inUnit, ok := freeableNow(unit, byName, drained, filled, movable)
			if !ok {
				continue
			}
			// Skip accelerator-empty units: freeing a node that runs no accelerator
			// pod isn't defrag (its accelerator capacity is already idle).
			accUnit := false
			for _, nn := range unit.Nodes {
				if occupiesAccelerator(byName[nn], res) {
					accUnit = true
					break
				}
			}
			if !accUnit {
				continue
			}
			var victims []*schedapi.TaskInfo
			for _, nn := range unit.Nodes {
				victims = append(victims, api.VictimsOf(byName[nn], movable)...)
			}
			if len(victims) == 0 {
				continue
			}
			receivers := make([]*schedapi.NodeInfo, 0, len(nodes))
			for _, n := range nodes {
				// Exclude the unit itself, already-drained nodes, and
				// accelerator-EMPTY nodes (0 pods requesting the accelerator — a
				// CPU/memory-only node counts as empty too): draining onto one just
				// relights a free accelerator node (net-zero shuffle). Full nodes (no
				// slack) are filtered by the solver, which also makes net-zero
				// full-node drains (pods only fit on an empty) infeasible.
				if inUnit[n.Name] || drained[n.Name] || !occupiesAccelerator(n, res) {
					continue
				}
				receivers = append(receivers, n)
			}
			dom := api.NewDomain(receivers, func(n *schedapi.NodeInfo) *schedapi.Resource { return ledger[n.Name] }, fit).Prefer(prefer)
			placed, feasible := dom.Feasible(victims)
			if !feasible {
				// Vacatability is monotonic (slack only shrinks), so a unit
				// infeasible now stays infeasible — cache it as a preferred receiver.
				for _, nn := range unit.Nodes {
					provenStuck[nn] = true
				}
				continue // cannot vacate this unit without orphaning a pod
			}
			// Disruption budget (maxPerRun): prospective deltas.
			newPGs := make(map[schedapi.JobID]bool)
			var newCards int64
			for _, v := range victims {
				if !movedPGs[v.Job] {
					newPGs[v.Job] = true
				}
				newCards += api.Scalar(v.InitResreq, res)
			}
			if mp := ssn.MaxPodGroups(); mp > 0 && len(movedPGs)+len(newPGs) > mp {
				continue
			}
			if mr := ssn.MaxResource(); mr > 0 && movedCards+newCards > mr {
				continue
			}
			feas = append(feas, cand{unit: unit, placed: placed, newPGs: newPGs, newCards: newCards, key: unitKey(unit)})
		}
		if len(feas) == 0 {
			break
		}
		// Deterministic order: higher benefit first (LeastDisruptive keeps the
		// earliest on ties, i.e. the highest-benefit), then by unit key.
		sort.SliceStable(feas, func(i, j int) bool {
			if feas[i].unit.Weight != feas[j].unit.Weight {
				return feas[i].unit.Weight > feas[j].unit.Weight
			}
			return feas[i].key < feas[j].key
		})
		cps := make([]*api.CandidatePlan, len(feas))
		for i, c := range feas {
			mv := make([]*api.Move, 0, len(moves)+len(c.placed))
			mv = append(mv, moves...)
			mv = append(mv, c.placed...)
			cps[i] = &api.CandidatePlan{Moves: mv}
		}
		chosen := feas[ssn.LeastDisruptive(cps)]

		// Commit the chosen unit.
		for _, m := range chosen.placed {
			if r := ledger[m.To]; r != nil {
				r.Sub(m.Task.InitResreq)
			}
			filled[m.To] = true
		}
		for pg := range chosen.newPGs {
			movedPGs[pg] = true
		}
		movedCards += chosen.newCards
		for _, nn := range chosen.unit.Nodes {
			drained[nn] = true
			freedNodes = append(freedNodes, nn)
		}
		freedUnits = append(freedUnits, chosen.unit)
		moves = append(moves, chosen.placed...)
	}

	if len(freedNodes) == 0 {
		return nil
	}
	return &api.RepackPlan{Moves: moves, FreedNodes: freedNodes, FreedUnits: freedUnits}
}

// freeableNow reports whether every node of the unit can still be a drain target
// (present, not already drained, not a receiver/filled, and freeable), returning
// the unit's node set.
func freeableNow(unit api.FreeableUnit, byName map[string]*schedapi.NodeInfo, drained, filled map[string]bool, movable api.Movable) (map[string]bool, bool) {
	inUnit := make(map[string]bool, len(unit.Nodes))
	for _, nn := range unit.Nodes {
		n := byName[nn]
		if n == nil || drained[nn] || filled[nn] || !api.NodeFreeable(n, movable) {
			return nil, false
		}
		inUnit[nn] = true
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
