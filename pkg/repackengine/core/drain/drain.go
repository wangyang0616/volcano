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
// gang-aware greedy. Each step cheaply ranks the active drain units against the
// committed moves, then lazily invokes the scheduler-faithful relocation solver
// in that order. The first feasible unit is committed. This preserves complete
// scheduler validation for the selected plan without paying that cost for every
// candidate in a large cluster. Because gang damage is scored over the whole
// prospective plan, a unit that reuses an already-affected gang remains cheap.
// Vacating a unit is atomic, bounded by the disruption budget, and the final plan
// is kept only when it meets the configured benefit constraints.
//
// "Unit" generalizes "node": a node-domain plugin yields one single-node unit per
// node; a hypernode-domain plugin yields multi-node units. With both enabled
// the units are a weighted union and the core prefers higher-benefit units first.
package drain

import (
	"cmp"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
)

func init() {
	framework.RegisterCore(framework.CoreDrain, func() framework.Core { return &drainCore{} })
}

type drainCore struct{}

const prospectiveReceiver = "<prospective>"

func (*drainCore) Name() string { return framework.CoreDrain }

// Plan runs the incremental, gang-aware drain over the session's freeable units.
func (*drainCore) Plan(ssn *framework.Session) (*api.RepackPlan, bool) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	targetResource := ssn.Resource()
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

	before := api.MeasureResourceFragmentation(nodes, targetResource)
	klog.V(3).InfoS("repack drain: planning pass started", "run", runName, "resource", targetResource,
		"nodes", len(nodes), "freeableUnits", len(units),
		"occupiedNodes", before.OccupiedNodeCount, "optimalNodes", before.OptimalOccupiedNodeCount,
		"providingNodes", before.ProvidingNodeCount)

	plan := drainGreedy(nodes, nodesByName, units, ssn, movable, targetResource)
	if plan == nil {
		klog.V(3).InfoS("repack drain: no plan produced", "run", runName, "resource", targetResource,
			"reason", "NoFreeableUnit")
		return nil, false
	}
	plan.Before = before
	// Hard admissibility gate: the built-in benefit constraints (MinNodesFreed,
	// MinFragImprovementPercent) plus any plugin-registered plan constraints (e.g.
	// disruptionPolicy.maxDisruptionScore, added later). A rejected plan = NoRepackNeeded.
	if !ssn.PlanAdmissible(plan) {
		klog.V(3).InfoS("repack drain: plan rejected by benefit gate (below MinNodesFreed / MinFragImprovement)",
			"run", runName, "resource", targetResource, "freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
			"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
		return nil, false
	}
	plan.Cost = api.CalculateDisruptionCost(plan.Moves, targetResource)
	klog.V(3).InfoS("repack drain: plan accepted", "run", runName, "resource", targetResource,
		"freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
		"movedResource", plan.Cost.MovedResource, "affectedPodGroupCount", plan.Cost.AffectedPodGroups,
		"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
	klog.V(4).InfoS("repack drain: accepted plan details", "run", runName, "freedNodes", plan.FreedNodes,
		"affectedPodGroups", plan.AffectedPodGroups())
	return plan, true
}

// candidate is one unit that can be vacated this step, with the moves that vacate
// it and the disruption-budget deltas those moves imply.
type candidate struct {
	unit               api.FreeableUnit
	inUnit             map[string]bool
	victims            []*schedapi.TaskInfo
	scoringMoves       []*api.Move
	placed             []*api.Move
	podGroups          map[schedapi.JobID]bool
	newPodGroups       map[schedapi.JobID]bool
	additionalResource int64
	key                string
}

// preparedUnit contains the immutable data collected once for a drain unit.
// active is monotonic: a unit leaves the set after it is proven infeasible,
// exceeds a monotonic budget, is drained, or receives a committed move.
type preparedUnit struct {
	candidate candidate
	active    bool
}

type scoredCandidate struct {
	candidate candidate
	score     framework.CandidateDisruptionScore
}

// drainState is the running state of one drain pass. Feasibility (which victims
// can relocate where) is delegated to the snapshot's scheduler-faithful feasibility check
// (Snapshot.FeasibleRelocation); this struct only tracks the greedy pass progress
// and the disruption budget.
type drainState struct {
	// Fixed for the whole pass.
	ssn                 *framework.Session
	snapshot            framework.Snapshot
	nodesByName         map[string]*schedapi.NodeInfo
	movable             api.Movable
	resource            v1.ResourceName
	maxPodGroups        int
	maxResource         int64
	hasPodGroupLimit    bool
	hasResourceLimit    bool
	alwaysStaysOccupied map[string]bool // immovable or outside drain scope
	receiverNodes       []*schedapi.NodeInfo
	receiverSlackByNode map[string]int64
	receiverResource    int64 // remaining target-resource slack across eligible receivers
	// futureDrainPodGroupsByNode caches the movable Pod/resource totals used to
	// assess each node as a possible next drain target.
	futureDrainPodGroupsByNode map[string]map[schedapi.JobID]api.PodGroupMoveAggregate

	// Mutated as units are committed.
	drained                 map[string]bool // emptied — no longer a receiver
	filled                  map[string]bool // received a moved-in pod
	provenStuck             map[string]bool // proven un-vacatable this pass → prefer as receiver
	stuckUnits              map[string]bool // monotonic infeasibility cache; never re-simulate
	movedPodGroups          map[schedapi.JobID]bool
	movedPodsByPodGroup     map[schedapi.JobID]int64
	movedResourceByPodGroup map[schedapi.JobID]int64
	movedResource           int64
	placedResourceByNode    map[string]int64 // resource already assigned to a receiver by committed moves
	candidatesEvaluated     int
	feasibilitySimulations  int
	prunedByReason          map[string]int
	activeUnits             []*preparedUnit
	moves                   []*api.Move
	freedNodes              []string
	freedUnits              []api.FreeableUnit
}

// drainGreedy is the single dynamic pass. Static unit facts are prepared once.
// Each step ranks active units with cheap prospective moves, then runs complete
// scheduler simulation lazily until the first feasible candidate is found.
// Terminates because each commit drains >= 1 node and inactive units never return.
func drainGreedy(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	units []api.FreeableUnit,
	ssn *framework.Session,
	movable api.Movable,
	targetResource v1.ResourceName,
) *api.RepackPlan {
	s := newDrainState(nodes, nodesByName, ssn, movable, targetResource)
	s.prepareUnits(units)
	planningStartTime := time.Now()
	defer func() {
		metrics.ObservePlanner(string(ssn.Mode()), s.candidatesEvaluated, s.feasibilitySimulations, s.prunedByReason)
		klog.V(4).InfoS("repack drain: planning performance summary", "run", runName(ssn),
			"candidateEvaluations", s.candidatesEvaluated, "feasibilitySimulations", s.feasibilitySimulations,
			"prunedByReason", s.prunedByReason, "duration", time.Since(planningStartTime))
	}()
	for step := 1; ; step++ {
		// 1. Rank all currently active candidates without constructing receiver
		// lists, cloning scheduler state, or running predicates.
		preliminary := s.preliminaryCandidates()
		if len(preliminary) == 0 {
			break
		}
		ordered := orderScoredCandidates(s.scoreCandidates(preliminary))
		klog.V(4).InfoS("repack drain: step ranked active units", "step", step,
			"activeCandidates", len(ordered), "nodesFreedSoFar", len(s.freedNodes))

		// 2. Lazily run the expensive scheduler simulation in disruption order.
		chosen, selectedPosition, ok := s.firstFeasibleCandidate(ordered)
		if !ok {
			break
		}
		s.logCandidateOrder(step, ordered, selectedPosition)
		// 3. Commit the first scheduler-feasible candidate.
		klog.V(4).InfoS("repack drain: committing unit", "step", step, "unit", chosen.key,
			"freesNodes", chosen.unit.Nodes, "moves", len(chosen.placed), "movedResource", chosen.additionalResource)
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
	targetResource v1.ResourceName,
) *drainState {
	state := &drainState{
		ssn:                        ssn,
		snapshot:                   ssn.Snapshot(),
		nodesByName:                nodesByName,
		movable:                    movable,
		resource:                   targetResource,
		maxPodGroups:               ssn.MaxPodGroups(),
		maxResource:                ssn.MaxResource(),
		hasPodGroupLimit:           ssn.LimitPodGroups(),
		hasResourceLimit:           ssn.LimitResource(),
		alwaysStaysOccupied:        make(map[string]bool, len(nodes)),
		receiverNodes:              make([]*schedapi.NodeInfo, 0, len(nodes)),
		receiverSlackByNode:        make(map[string]int64, len(nodes)),
		futureDrainPodGroupsByNode: make(map[string]map[schedapi.JobID]api.PodGroupMoveAggregate, len(nodes)),
		drained:                    make(map[string]bool),
		filled:                     make(map[string]bool),
		provenStuck:                make(map[string]bool),
		stuckUnits:                 make(map[string]bool),
		movedPodGroups:             make(map[schedapi.JobID]bool),
		movedPodsByPodGroup:        make(map[schedapi.JobID]int64),
		movedResourceByPodGroup:    make(map[schedapi.JobID]int64),
		placedResourceByNode:       make(map[string]int64),
		prunedByReason:             make(map[string]int),
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		state.alwaysStaysOccupied[node.Name] =
			!api.EvaluateNodeFreeability(node, api.NodeFreeabilityState{}, movable, targetResource).Freeable ||
				!state.snapshot.NodeInScope(node)
		state.futureDrainPodGroupsByNode[node.Name] = aggregateTasksByPodGroup(
			api.VictimsOf(node, movable, targetResource), targetResource,
		)
		if occupiesAccelerator(node, targetResource) {
			state.receiverNodes = append(state.receiverNodes, node)
			slack := receiverSlack(node, targetResource)
			state.receiverSlackByNode[node.Name] = slack
			state.receiverResource += max(slack, 0)
		}
	}
	return state
}

// prepareUnits performs all snapshot-stable drain checks and materializes the
// victims/scoring moves once. Dynamic state and budgets are handled per step.
func (s *drainState) prepareUnits(units []api.FreeableUnit) {
	s.activeUnits = make([]*preparedUnit, 0, len(units))
	for _, unit := range units {
		if prepared, ok := s.prepareUnit(unit); ok {
			s.activeUnits = append(s.activeUnits, &preparedUnit{candidate: prepared, active: true})
		}
	}
}

func (s *drainState) prepareUnit(unit api.FreeableUnit) (candidate, bool) {
	key := unitKey(unit)
	inUnit, diagnostics, ok := freeableNow(unit, s.nodesByName, nil, nil, s.movable, s.resource)
	if !ok {
		logNonFreeableNodes(key, diagnostics, s.resource)
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
	// Calculate cheap, deterministic prospective deltas before invoking the full
	// scheduler simulation. Under a tight maxPerRun this avoids cloning cycle
	// state and running predicates for candidates that cannot be selected anyway.
	podGroups := make(map[schedapi.JobID]bool)
	var additionalResource int64
	for _, v := range victims {
		podGroups[v.Job] = true
		additionalResource += api.Scalar(v.InitResreq, s.resource)
	}
	scoringMoves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		scoringMoves = append(scoringMoves, &api.Move{Task: victim, From: victim.NodeName, To: prospectiveReceiver})
	}
	return candidate{
		unit: unit, inUnit: inUnit, victims: victims, scoringMoves: scoringMoves,
		podGroups: podGroups, additionalResource: additionalResource, key: key,
	}, true
}

// preliminaryCandidates applies cheap, monotonic state, budget, and aggregate
// receiver-capacity checks. Candidates rejected here never enter disruption
// scoring and are removed permanently from the active set.
func (s *drainState) preliminaryCandidates() []candidate {
	preliminary := make([]candidate, 0, len(s.activeUnits))
	active := s.activeUnits[:0]
	for _, prepared := range s.activeUnits {
		if !prepared.active {
			continue
		}
		candidate := prepared.candidate
		if s.stuckUnits[candidate.key] {
			prepared.active = false
			continue
		}
		if s.unitConsumed(candidate.unit) {
			prepared.active = false
			continue
		}
		s.candidatesEvaluated++
		candidate.newPodGroups = make(map[schedapi.JobID]bool)
		for podGroup := range candidate.podGroups {
			if !s.movedPodGroups[podGroup] {
				candidate.newPodGroups[podGroup] = true
			}
		}
		if (s.hasPodGroupLimit || s.maxPodGroups > 0) && len(s.movedPodGroups)+len(candidate.newPodGroups) > s.maxPodGroups {
			s.recordPruned("max_pod_groups")
			prepared.active = false
			continue
		}
		if (s.hasResourceLimit || s.maxResource > 0) && s.movedResource+candidate.additionalResource > s.maxResource {
			s.recordPruned("max_resource")
			prepared.active = false
			continue
		}
		if !s.hasAggregateReceiverCapacity(candidate) {
			s.recordPruned("insufficient_receiver_resource")
			klog.V(5).InfoS("repack drain: skip unit — aggregate receiver resource capacity is insufficient",
				"unit", candidate.key, "required", candidate.additionalResource)
			s.markUnitInfeasible(candidate.key, candidate.unit)
			prepared.active = false
			continue
		}
		preliminary = append(preliminary, candidate)
		active = append(active, prepared)
	}
	s.activeUnits = active
	return preliminary
}

func (s *drainState) unitConsumed(unit api.FreeableUnit) bool {
	for _, nodeName := range unit.Nodes {
		if s.drained[nodeName] || s.filled[nodeName] {
			return true
		}
	}
	return false
}

// firstFeasibleCandidate is the lazy boundary: candidates are already ordered
// by cheap disruption scoring, and complete scheduler simulation stops at the
// first success. selectedPosition is one-based within the preliminary order.
func (s *drainState) firstFeasibleCandidate(ordered []scoredCandidate) (candidate, int, bool) {
	for index := range ordered {
		candidate := ordered[index].candidate
		receivers := s.receiversInPreferenceOrder(candidate.inUnit, candidate.victims)
		if !s.receiversHaveResourceCapacity(receivers, candidate.additionalResource) {
			s.recordPruned("insufficient_receiver_resource")
			klog.V(5).InfoS("repack drain: skip unit — receiver resource capacity is insufficient", "unit", candidate.key,
				"required", candidate.additionalResource, "receivers", len(receivers))
			s.markUnitInfeasible(candidate.key, candidate.unit)
			continue
		}
		klog.V(5).InfoS("repack drain: evaluating unit feasibility", "unit", candidate.key,
			"victims", taskNames(candidate.victims), "victimCount", len(candidate.victims),
			"receivers", nodeNames(receivers), "receiverCount", len(receivers))
		s.feasibilitySimulations++
		placed, feasible := s.snapshot.FeasibleRelocation(s.moves, candidate.victims, receivers)
		if !feasible {
			klog.V(4).InfoS("repack drain: unit INFEASIBLE — victims cannot all relocate onto receivers",
				"unit", candidate.key, "victims", len(candidate.victims), "receivers", len(receivers))
			s.markUnitInfeasible(candidate.key, candidate.unit)
			s.recordPruned("scheduler_infeasible")
			continue
		}
		candidate.placed = placed
		if klog.V(5).Enabled() {
			for _, move := range placed {
				klog.V(5).InfoS("repack drain: victim placement", "unit", candidate.key,
					"pod", move.Task.Name, "from", move.From, "to", move.To)
			}
		}
		return candidate, index + 1, true
	}
	return candidate{}, 0, false
}

// hasAggregateReceiverCapacity rejects an impossible drain in O(unit size)
// before constructing and sorting an O(cluster size) receiver list. It is only
// a necessary target-resource check; full scheduler predicates remain decisive.
func (s *drainState) hasAggregateReceiverCapacity(candidate candidate) bool {
	available := s.receiverResource
	for nodeName := range candidate.inUnit {
		available -= max(s.receiverSlackByNode[nodeName]-s.placedResourceByNode[nodeName], 0)
	}
	return available >= candidate.additionalResource
}

func (s *drainState) recordPruned(reason string) {
	s.prunedByReason[reason]++
}

func runName(ssn *framework.Session) string {
	if run := ssn.Run(); run != nil {
		return run.Name
	}
	return ""
}

// receiversHaveResourceCapacity is a necessary, intentionally predicate-free
// preflight. It accounts for moves already committed to a receiver, then lets
// FeasibleRelocation perform the authoritative multi-dimensional filter check.
func (s *drainState) receiversHaveResourceCapacity(receivers []*schedapi.NodeInfo, requiredResource int64) bool {
	var availableResource int64
	for _, receiver := range receivers {
		availableResource += s.receiverSlackByNode[receiver.Name] - s.placedResourceByNode[receiver.Name]
		if availableResource >= requiredResource {
			return true
		}
	}
	return false
}

func (s *drainState) markUnitInfeasible(key string, unit api.FreeableUnit) {
	for _, nodeName := range unit.Nodes {
		s.provenStuck[nodeName] = true
	}
	s.stuckUnits[key] = true
}

type receiverPreference struct {
	node              *schedapi.NodeInfo
	staysOccupied     bool
	futureDrainImpact futureDrainImpact
	availableResource int64
}

// futureDrainImpact describes the incremental disruption caused by draining a
// receiver after the current candidate. Larger values make a node more useful
// as a receiver now, because preserving it as a future target would be costlier.
type futureDrainImpact struct {
	additionalGangBreaches      int
	additionalAffectedPodGroups int
	additionalDamagedResource   int64
	movedResource               int64
	movedPods                   int64
}

// receiversInPreferenceOrder returns the nodes that may receive relocated pods,
// in the order the drain prefers to fill them. A node is eligible if it is not
// in the unit being vacated, not already drained, and not accelerator-empty (0
// pods requesting the accelerator — a CPU/memory-only node counts as empty too).
// Moving onto an empty accelerator node would merely relight it and provide no
// consolidation benefit.
//
// The preference is intentionally soft; FeasibleRelocation may skip an earlier
// receiver that lacks capacity or fails a scheduler predicate:
//   - Nodes that cannot be drained later are filled first.
//   - Among still-drainable nodes, hypothetically drain each receiver after the
//     current candidate. Prefer receivers whose future drain would first add
//     minAvailable breaches, then affect more PodGroups or damage more resource.
//     This consumes expensive future targets and preserves cheaper ones.
//   - Within an equal disruption tier, use best-fit so relocations consolidate
//     onto the receiver with the least remaining target resource (§4.9).
func (s *drainState) receiversInPreferenceOrder(
	inUnit map[string]bool,
	candidateVictims []*schedapi.TaskInfo,
) []*schedapi.NodeInfo {
	candidatePodGroupMoves := aggregateTasksByPodGroup(candidateVictims, s.resource)
	preferences := make([]receiverPreference, 0, len(s.receiverNodes))
	minimumVictimResource := minTaskResource(candidateVictims, s.resource)
	for _, n := range s.receiverNodes {
		if inUnit[n.Name] || s.drained[n.Name] {
			continue
		}
		availableResource := s.receiverSlackByNode[n.Name] - s.placedResourceByNode[n.Name]
		// Every victim requests the target resource. A receiver that cannot fit
		// even the smallest victim can never be selected by the full solver.
		if availableResource < minimumVictimResource {
			continue
		}
		staysOccupied := s.staysOccupied(n)
		impact := futureDrainImpact{}
		if !staysOccupied {
			impact = s.measureFutureDrainImpact(n, candidatePodGroupMoves)
		}
		preferences = append(preferences, receiverPreference{
			node:              n,
			staysOccupied:     staysOccupied,
			futureDrainImpact: impact,
			availableResource: availableResource,
		})
	}
	sort.SliceStable(preferences, func(i, j int) bool {
		if preferences[i].staysOccupied != preferences[j].staysOccupied {
			return preferences[i].staysOccupied
		}
		if comparison := compareFutureDrainImpact(
			preferences[i].futureDrainImpact,
			preferences[j].futureDrainImpact,
		); comparison != 0 {
			return comparison > 0
		}
		return preferences[i].availableResource < preferences[j].availableResource
	})
	if klog.V(5).Enabled() && len(preferences) > 0 {
		preferred := preferences[0]
		klog.V(5).InfoS("repack drain: preferred receiver calculated",
			"candidateVictims", taskNames(candidateVictims),
			"receiverCount", len(preferences),
			"preferredReceiver", preferred.node.Name,
			"staysOccupied", preferred.staysOccupied,
			"futureAdditionalGangBreaches", preferred.futureDrainImpact.additionalGangBreaches,
			"futureAdditionalAffectedPodGroups", preferred.futureDrainImpact.additionalAffectedPodGroups,
			"futureAdditionalDamagedResource", preferred.futureDrainImpact.additionalDamagedResource,
			"availableResource", preferred.availableResource)
	}

	receivers := make([]*schedapi.NodeInfo, 0, len(preferences))
	for _, preference := range preferences {
		receivers = append(receivers, preference.node)
	}
	return receivers
}

func minTaskResource(tasks []*schedapi.TaskInfo, targetResource v1.ResourceName) int64 {
	var minimum int64
	for _, task := range tasks {
		requested := api.Scalar(task.InitResreq, targetResource)
		if requested > 0 && (minimum == 0 || requested < minimum) {
			minimum = requested
		}
	}
	return minimum
}

// measureFutureDrainImpact compares the prospective plan before and after the
// receiver's own movable Pods are added. MeasurePodGroupDisruption is shared
// with the gang plugin, so both paths use exactly the same minAvailable rule.
func (s *drainState) measureFutureDrainImpact(
	node *schedapi.NodeInfo,
	candidatePodGroupMoves map[schedapi.JobID]api.PodGroupMoveAggregate,
) futureDrainImpact {
	impact := futureDrainImpact{}
	for podGroup, futureMoves := range s.futureDrainPodGroupsByNode[node.Name] {
		candidateMoves := candidatePodGroupMoves[podGroup]
		movedPodsBefore := s.movedPodsByPodGroup[podGroup] + candidateMoves.MovedPods
		movedResourceBefore := s.movedResourceByPodGroup[podGroup] + candidateMoves.MovedResource
		movedPodsAfter := movedPodsBefore + futureMoves.MovedPods
		movedResourceAfter := movedResourceBefore + futureMoves.MovedResource

		if movedPodsBefore == 0 {
			impact.additionalAffectedPodGroups++
		}
		view := s.snapshot.PodGroupView(podGroup)
		before := api.MeasurePodGroupDisruption(view, movedPodsBefore, movedResourceBefore)
		after := api.MeasurePodGroupDisruption(view, movedPodsAfter, movedResourceAfter)
		if !before.Breached && after.Breached {
			impact.additionalGangBreaches++
		}
		impact.additionalDamagedResource += after.DamagedResource - before.DamagedResource
		impact.movedPods += futureMoves.MovedPods
		impact.movedResource += futureMoves.MovedResource
	}
	return impact
}

// compareFutureDrainImpact returns a positive value when left would be the more
// disruptive future drain target. Gang safety is compared first, followed by
// blast radius and move size; equal impact falls back to receiver best-fit.
func compareFutureDrainImpact(left, right futureDrainImpact) int {
	switch {
	case left.additionalGangBreaches != right.additionalGangBreaches:
		return cmp.Compare(left.additionalGangBreaches, right.additionalGangBreaches)
	case left.additionalAffectedPodGroups != right.additionalAffectedPodGroups:
		return cmp.Compare(left.additionalAffectedPodGroups, right.additionalAffectedPodGroups)
	case left.additionalDamagedResource != right.additionalDamagedResource:
		return cmp.Compare(left.additionalDamagedResource, right.additionalDamagedResource)
	case left.movedResource != right.movedResource:
		return cmp.Compare(left.movedResource, right.movedResource)
	default:
		return cmp.Compare(left.movedPods, right.movedPods)
	}
}

func aggregateTasksByPodGroup(
	tasks []*schedapi.TaskInfo,
	targetResource v1.ResourceName,
) map[schedapi.JobID]api.PodGroupMoveAggregate {
	aggregates := make(map[schedapi.JobID]api.PodGroupMoveAggregate)
	for _, task := range tasks {
		if task == nil || task.Job == "" {
			continue
		}
		aggregate := aggregates[task.Job]
		aggregate.MovedPods++
		aggregate.MovedResource += api.Scalar(task.InitResreq, targetResource)
		aggregates[task.Job] = aggregate
	}
	return aggregates
}

// receiverSlack is the target-resource free capacity used to best-fit sort receivers.
// Prefer FutureIdle (scheduler cache); fall back to Allocatable−Used for test nodes
// that only set Used/Allocatable without initializing Idle.
func receiverSlack(n *schedapi.NodeInfo, targetResource v1.ResourceName) int64 {
	if n == nil {
		return 0
	}
	if n.Idle != nil {
		return api.Scalar(n.FutureIdle(), targetResource)
	}
	if n.Allocatable == nil {
		return 0
	}
	free := n.Allocatable.Clone()
	if n.Used != nil {
		free.SubWithoutAssert(n.Used)
	}
	return api.Scalar(free, targetResource)
}

// staysOccupied reports whether a node will remain occupied regardless of this pass,
// so it is a preferred receiver. A node stays if it already received a committed
// move, has an immovable accelerator pod, is receiver-only (excluded from
// draining by scope.nodes), or was already proven un-vacatable this pass.
func (s *drainState) staysOccupied(n *schedapi.NodeInfo) bool {
	return s.filled[n.Name] ||
		s.alwaysStaysOccupied[n.Name] ||
		s.provenStuck[n.Name]
}

// scoreCandidates orders active candidates by deterministic tie-breakers, then
// evaluates their preliminary disruption scores. Stable score ordering preserves
// higher unit benefit and lexical unit key when scores tie.
func (s *drainState) scoreCandidates(activeCandidates []candidate) []scoredCandidate {
	orderedCandidates := append([]candidate(nil), activeCandidates...)
	sort.SliceStable(orderedCandidates, func(i, j int) bool {
		if orderedCandidates[i].unit.Weight != orderedCandidates[j].unit.Weight {
			return orderedCandidates[i].unit.Weight > orderedCandidates[j].unit.Weight
		}
		return orderedCandidates[i].key < orderedCandidates[j].key
	})
	candidatePlans := make([]*api.CandidatePlan, len(orderedCandidates))
	for index, candidate := range orderedCandidates {
		moves := candidate.scoringMoves
		if len(moves) == 0 {
			moves = candidate.placed // direct unit tests and already-realized candidates
		}
		candidatePlans[index] = &api.CandidatePlan{CommittedMoves: s.moves, Moves: moves}
	}
	scores := s.ssn.DisruptionScores(candidatePlans)
	scored := make([]scoredCandidate, len(orderedCandidates))
	for index := range orderedCandidates {
		scored[index] = scoredCandidate{candidate: orderedCandidates[index], score: scores[index]}
	}
	return scored
}

func orderScoredCandidates(scored []scoredCandidate) []scoredCandidate {
	ordered := append([]scoredCandidate(nil), scored...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].score.Total < ordered[j].score.Total
	})
	return ordered
}

// commit applies the chosen candidate to the pass state: mark receivers filled and
// the unit's nodes drained, record moved PodGroups/cards, and append the moves.
func (s *drainState) commit(chosen candidate) {
	for _, m := range chosen.placed {
		s.filled[m.To] = true
		if m.Task != nil {
			movedResource := api.Scalar(m.Task.InitResreq, s.resource)
			s.placedResourceByNode[m.To] += movedResource
			s.receiverResource -= movedResource
			s.movedPodsByPodGroup[m.Task.Job]++
			s.movedResourceByPodGroup[m.Task.Job] += movedResource
		}
	}
	for pg := range chosen.newPodGroups {
		s.movedPodGroups[pg] = true
	}
	s.movedResource += chosen.additionalResource
	for _, nodeName := range chosen.unit.Nodes {
		s.drained[nodeName] = true
		s.receiverResource -= max(s.receiverSlackByNode[nodeName]-s.placedResourceByNode[nodeName], 0)
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

type nodeFreeability struct {
	nodeName string
	result   api.NodeFreeability
}

// freeableNow evaluates every node in a unit exactly once. It returns the
// blocking diagnostics alongside the eligibility result, so logging cannot
// diverge from the decision or require a second task scan.
func freeableNow(unit api.FreeableUnit, nodesByName map[string]*schedapi.NodeInfo, drained, filled map[string]bool, movable api.Movable, targetResource v1.ResourceName) (map[string]bool, []nodeFreeability, bool) {
	inUnit := make(map[string]bool, len(unit.Nodes))
	var diagnostics []nodeFreeability
	for _, nodeName := range unit.Nodes {
		result := api.EvaluateNodeFreeability(nodesByName[nodeName], api.NodeFreeabilityState{
			Drained: drained[nodeName],
			Filled:  filled[nodeName],
		}, movable, targetResource)
		if !result.Freeable {
			diagnostics = append(diagnostics, nodeFreeability{nodeName: nodeName, result: result})
			continue
		}
		inUnit[nodeName] = true
	}
	return inUnit, diagnostics, len(diagnostics) == 0 && len(inUnit) > 0
}

// logNonFreeableNodes writes one V(4) record for every node that prevents a
// freeable unit from being drained. The diagnostics are produced by the same
// EvaluateNodeFreeability call that rejected the unit.
func logNonFreeableNodes(unitKey string, diagnostics []nodeFreeability, targetResource v1.ResourceName) {
	for _, diagnostic := range diagnostics {
		immovablePods := make([]string, 0, len(diagnostic.result.ImmovableTasks))
		for _, task := range diagnostic.result.ImmovableTasks {
			pod := task.Namespace + "/" + task.Name
			if task.Job != "" {
				pod += " (podGroup=" + string(task.Job) + ")"
			} else {
				pod += " (podGroup=<none>)"
			}
			immovablePods = append(immovablePods, pod)
		}
		sort.Strings(immovablePods)
		klog.V(4).InfoS("repack drain: node cannot be freed", "unit", unitKey, "node", diagnostic.nodeName,
			"targetResource", targetResource, "reason", diagnostic.result.Reason, "immovablePods", immovablePods)
	}
}

// occupiesAccelerator reports whether the node uses any of the target accelerator
// resource — the SAME criterion MeasureResourceFragmentation uses to count a node as occupied
// (B), so "the drain treats this node as empty" ⟺ "the fragmentation metric does
// not count it". A node with only CPU/memory pods is empty for defrag: its
// accelerator capacity is idle, so filling it just lights up a fresh accelerator
// node (net-zero), and freeing it is not a consolidation. Everything (empty/full/
// fragmentation) is judged by the resource being defragmented (goals[0].resource).
func occupiesAccelerator(n *schedapi.NodeInfo, targetResource v1.ResourceName) bool {
	return n != nil && n.Used != nil && api.Scalar(n.Used, targetResource) > 0
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
