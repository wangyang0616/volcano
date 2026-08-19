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

// Package gang is the gang-awareness plugin: it registers the gang-semantics
// disruption scores so the planner prefers plans that don't shatter PodGroups —
// gangBreaches (gangs pushed below minAvailable) and damagedResource (a breached gang
// counts its WHOLE footprint as lost, capturing "突破 minAvailable 即整组受损").
package gang

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "gang"

// Default weights for the gang-semantics disruption dimensions: pushing a gang
// below minAvailable (a breach) is weighted above merely churning a breached gang's
// cards (disruptionPolicy overrides these later).
const (
	weightGangBreaches    = 0.8
	weightDamagedResource = 0.6
	receiverRankPriority  = 20
)

func init() {
	framework.RegisterPlugin(Name, func() framework.Plugin { return &gangPlugin{} })
}

type gangPlugin struct{}

func (*gangPlugin) Name() string { return Name }

func (*gangPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddDisruptionScoreFn("gangBreaches", weightGangBreaches, scoreGangBreaches)
	ssn.AddDisruptionScoreFn("damagedResource", weightDamagedResource, scoreDamagedResource)
	ssn.AddReceiverRankFn("futureGangImpact", receiverRankPriority, scoreFutureReceiverImpact)
}

func (*gangPlugin) OnSessionClose(*framework.Session) {}

// scoreGangBreaches: number of gangs pushed below minAvailable (moved pods exceed
// the gang's slack = Running − MinAvailable), risking gang eviction mid-repack.
func scoreGangBreaches(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var breachedGangs int64
	for podGroupID, moved := range p.MoveAggregate(ctx).ByPodGroup {
		disruption := api.MeasurePodGroupDisruption(
			ctx.PodGroupView(podGroupID), moved.MovedPods, moved.MovedResource,
		)
		if disruption.Breached {
			breachedGangs++
		}
	}
	return float64(breachedGangs)
}

// scoreDamagedResource: gang-semantics "damaged resource". Within slack → only the moved
// cards count; breaching minAvailable → the whole gang Footprint counts (and
// further pods of an already-breached gang add nothing).
func scoreDamagedResource(ctx *api.PlanContext, p *api.CandidatePlan) float64 {
	var damagedResource int64
	for podGroupID, moved := range p.MoveAggregate(ctx).ByPodGroup {
		disruption := api.MeasurePodGroupDisruption(
			ctx.PodGroupView(podGroupID), moved.MovedPods, moved.MovedResource,
		)
		damagedResource += disruption.DamagedResource
	}
	return float64(damagedResource)
}

// scoreFutureReceiverImpact prefers consuming a node whose own later drain would
// add more disruption. The rank preserves the old strict comparison order:
// breaches, affected PodGroups, damaged resource, moved resource, moved Pods.
func scoreFutureReceiverImpact(ctx *api.PlanContext, candidate *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverRank {
	if candidate == nil || candidate.Plan == nil || receiver == nil || receiver.StaysOccupied {
		return framework.ReceiverRank{}
	}
	aggregate := candidate.Plan.MoveAggregate(ctx)
	var breaches, affected, damagedResource, movedResource, movedPods int64
	for podGroup, futureMoves := range receiver.FutureMoves {
		beforeMoves := aggregate.ByPodGroup[podGroup]
		var movedPodsBefore, movedResourceBefore int64
		if beforeMoves != nil {
			movedPodsBefore = beforeMoves.MovedPods
			movedResourceBefore = beforeMoves.MovedResource
		}
		afterMovedPods := movedPodsBefore + futureMoves.MovedPods
		afterMovedResource := movedResourceBefore + futureMoves.MovedResource
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
		movedResource += futureMoves.MovedResource
		movedPods += futureMoves.MovedPods
	}
	return framework.ReceiverRank{breaches, affected, damagedResource, movedResource, movedPods}
}
