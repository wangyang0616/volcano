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

// Package gangdisruption is the gang-awareness plugin: it registers the gang-semantics
// disruption scores so the planner prefers plans that don't shatter PodGroups —
// gangBreaches (gangs pushed below minAvailable) and damagedResource (a breached gang
// counts its WHOLE footprint as lost, capturing "突破 minAvailable 即整组受损").
package gangdisruption

import (
	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "gangdisruption"

// Default weights for the gang-semantics disruption dimensions. repack-conf
// plugin arguments override them; a zero weight disables the corresponding term.
const (
	weightGangBreaches    int64 = 8
	weightDamagedResource int64 = 6

	argGangBreachesWeight    = "gangBreachesWeight"
	argDamagedResourceWeight = "damagedResourceWeight"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: newPlugin, Validator: validateArguments,
	})
}

type gangDisruptionPlugin struct {
	gangBreachesWeight    int64
	damagedResourceWeight int64
	futureMovesByNode     map[string]map[schedapi.JobID]api.PodGroupMoveAggregate
	futureMovable         api.Movable
	futureResource        v1.ResourceName
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &gangDisruptionPlugin{
		gangBreachesWeight:    configuredWeight(arguments, argGangBreachesWeight, weightGangBreaches),
		damagedResourceWeight: configuredWeight(arguments, argDamagedResourceWeight, weightDamagedResource),
	}
}

func configuredWeight(arguments framework.Arguments, key string, defaultValue int64) int64 {
	value, err := arguments.NonNegativeInt(key, defaultValue)
	if err != nil {
		return defaultValue
	}
	return value
}

func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argGangBreachesWeight, argDamagedResourceWeight); err != nil {
		return err
	}
	if _, err := arguments.NonNegativeInt(argGangBreachesWeight, weightGangBreaches); err != nil {
		return err
	}
	if _, err := arguments.NonNegativeInt(argDamagedResourceWeight, weightDamagedResource); err != nil {
		return err
	}
	return nil
}

func (*gangDisruptionPlugin) Name() string { return Name }

func (p *gangDisruptionPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDisruptionScoreFn("gangBreaches", p.gangBreachesWeight, scoreGangBreaches)
	ssn.AddDisruptionScoreFn("damagedResource", p.damagedResourceWeight, scoreDamagedResource)
	ssn.AddReceiverRankFn("futureGangImpact", framework.ReceiverRankPhaseDisruption,
		func(ctx *api.PlanContext, candidate *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverRank {
			var futureMoves map[schedapi.JobID]api.PodGroupMoveAggregate
			if receiver != nil && receiver.Node != nil {
				futureMoves = p.futureMovesForReceiver(ssn, receiver.Node)
			}
			return scoreFutureReceiverImpact(ctx, candidate, receiver, futureMoves)
		})
}

func (p *gangDisruptionPlugin) OnSessionClose(*framework.Session) {
	p.futureMovesByNode = nil
	p.futureMovable = nil
	p.futureResource = ""
}

// futureMovesForReceiver builds the Gang look-ahead cache on demand for nodes
// that actually reach receiver ranking. Empty, full, unavailable, and filtered
// nodes therefore incur no task scan or cache allocation. Initialization remains
// lazy so every plugin has completed OnSessionOpen before the composed Movable
// policy is captured.
func (p *gangDisruptionPlugin) futureMovesForReceiver(
	ssn *framework.Session,
	node *schedapi.NodeInfo,
) map[schedapi.JobID]api.PodGroupMoveAggregate {
	if node == nil {
		return nil
	}
	if p.futureMovesByNode != nil {
		if futureMoves, found := p.futureMovesByNode[node.Name]; found {
			return futureMoves
		}
	} else {
		p.futureMovesByNode = make(map[string]map[schedapi.JobID]api.PodGroupMoveAggregate)
		p.futureMovable = ssn.Movable()
		p.futureResource = ssn.Resource()
	}
	futureMoves := aggregateTasksByPodGroup(
		api.VictimsOf(node, p.futureMovable, p.futureResource), p.futureResource,
	)
	p.futureMovesByNode[node.Name] = futureMoves
	return futureMoves
}

// scoreGangBreaches: number of gangs pushed below minAvailable (moved pods exceed
// the gang's slack = Running − MinAvailable), risking gang eviction mid-repack.
func scoreGangBreaches(ctx *api.PlanContext, p *api.CandidatePlan) int64 {
	var breachedGangs int64
	for podGroupID, moved := range p.MoveAggregate(ctx).ByPodGroup {
		disruption := api.MeasurePodGroupDisruption(
			ctx.PodGroupView(podGroupID), moved.MovedPods, moved.MovedResource,
		)
		if disruption.Breached {
			breachedGangs++
		}
	}
	return breachedGangs
}

// scoreDamagedResource: gang-semantics "damaged resource". Within slack → only the moved
// cards count; breaching minAvailable → the whole gang Footprint counts (and
// further pods of an already-breached gang add nothing).
func scoreDamagedResource(ctx *api.PlanContext, p *api.CandidatePlan) int64 {
	var damagedResource int64
	for podGroupID, moved := range p.MoveAggregate(ctx).ByPodGroup {
		disruption := api.MeasurePodGroupDisruption(
			ctx.PodGroupView(podGroupID), moved.MovedPods, moved.MovedResource,
		)
		damagedResource += disruption.DamagedResource
	}
	return damagedResource
}

// scoreFutureReceiverImpact prefers consuming a node whose own later drain would
// add more disruption. The rank preserves the old strict comparison order:
// breaches, affected PodGroups, damaged resource, moved resource, moved Pods.
func scoreFutureReceiverImpact(
	ctx *api.PlanContext,
	candidate *framework.PlanningCandidate,
	receiver *framework.ReceiverCandidate,
	futureMoves map[schedapi.JobID]api.PodGroupMoveAggregate,
) framework.ReceiverRank {
	if candidate == nil || candidate.Plan == nil || receiver == nil || receiver.StaysOccupied {
		return framework.ReceiverRank{}
	}
	aggregate := candidate.Plan.MoveAggregate(ctx)
	var breaches, affected, damagedResource, movedResource, movedPods int64
	for podGroup, futureMove := range futureMoves {
		beforeMoves := aggregate.ByPodGroup[podGroup]
		var movedPodsBefore, movedResourceBefore int64
		if beforeMoves != nil {
			movedPodsBefore = beforeMoves.MovedPods
			movedResourceBefore = beforeMoves.MovedResource
		}
		afterMovedPods := movedPodsBefore + futureMove.MovedPods
		afterMovedResource := movedResourceBefore + futureMove.MovedResource
		if movedPodsBefore == 0 {
			affected++
		}
		view := ctx.PodGroupView(podGroup)
		before := api.MeasurePodGroupDisruption(view, movedPodsBefore, movedResourceBefore)
		after := api.MeasurePodGroupDisruption(view, afterMovedPods, afterMovedResource)
		if !before.Breached && after.Breached {
			breaches++
		}
		damagedResource += after.DamagedResource - before.DamagedResource
		movedResource += futureMove.MovedResource
		movedPods += futureMove.MovedPods
	}
	return framework.ReceiverRank{breaches, affected, damagedResource, movedResource, movedPods}
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
