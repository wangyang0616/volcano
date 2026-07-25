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

package api

import (
	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// PodGroupView exposes the gang/priority facts a disruption strategy needs about
// one PodGroup. The session fills it from api.JobInfo; tests fake it.
type PodGroupView struct {
	MinAvailable int32 // gang floor: this many pods must stay running
	Running      int32 // pods currently running for the PodGroup
	Priority     int32 // PodGroup priority (higher = more important to keep)
	Footprint    int64 // gang's total accelerator cards (whole-gang blast radius)
}

// PodGroupViewer supplies disruption-scoring facts.
type PodGroupViewer interface {
	PodGroupView(id api.JobID) PodGroupView
}

// PlanContext carries lookups shared by every scoring strategy in a comparison.
// It is built by the engine Session and passed to the disruption score functions
// that plugins register.
type PlanContext struct {
	TargetResource v1.ResourceName // accelerator resource being defragmented
	PodGroupViews  PodGroupViewer  // per-PodGroup facts; nil-safe via PodGroupView()
}

// PodGroupView returns the PodGroup facts, zero-valued when unknown.
func (context *PlanContext) PodGroupView(podGroupID api.JobID) PodGroupView {
	if context == nil || context.PodGroupViews == nil {
		return PodGroupView{}
	}
	return context.PodGroupViews.PodGroupView(podGroupID)
}

// Resource returns the target accelerator, defaulting to nvidia.com/gpu.
func (context *PlanContext) Resource() v1.ResourceName {
	if context == nil || context.TargetResource == "" {
		return v1.ResourceName("nvidia.com/gpu")
	}
	return context.TargetResource
}

// CandidatePlan is one rearrangement under comparison. CommittedMoves are the
// moves already selected in this planning pass; Moves are the candidate's
// incremental moves. Keeping the slices separate avoids copying the growing
// committed prefix for every candidate during disruption scoring. Aggregates are
// computed once and cached.
type CandidatePlan struct {
	CommittedMoves []*Move
	Moves          []*Move
	aggregate      *PlanMoveAggregate
}

// PodGroupMoveAggregate is the per-PodGroup move aggregate.
type PodGroupMoveAggregate struct {
	MovedPods     int64
	MovedResource int64
}

// PodGroupDisruption is the gang impact of moving some Pods from one PodGroup.
type PodGroupDisruption struct {
	Breached        bool
	DamagedResource int64
}

// MeasurePodGroupDisruption applies the shared minAvailable semantics used by
// both plan scoring and drain receiver preference. Moves within the PodGroup's
// running slack damage only the moved resource; once minAvailable is breached,
// the whole PodGroup footprint is considered damaged.
func MeasurePodGroupDisruption(view PodGroupView, movedPods, movedResource int64) PodGroupDisruption {
	slack := int64(view.Running) - int64(view.MinAvailable)
	if slack < 0 {
		slack = 0
	}
	if movedPods > slack {
		return PodGroupDisruption{Breached: true, DamagedResource: view.Footprint}
	}
	return PodGroupDisruption{DamagedResource: movedResource}
}

// PlanMoveAggregate is the whole-plan move aggregate, with a per-PodGroup breakdown.
type PlanMoveAggregate struct {
	AffectedPodGroups int64
	MovedResource     int64
	MovedPods         int64
	ByPodGroup        map[api.JobID]*PodGroupMoveAggregate
}

// MoveAggregate computes (and caches) the move aggregate for the given context.
// Exported so disruption score functions in plugin packages can build on it.
func (plan *CandidatePlan) MoveAggregate(context *PlanContext) *PlanMoveAggregate {
	if plan.aggregate != nil {
		return plan.aggregate
	}
	moveAggregate := &PlanMoveAggregate{ByPodGroup: map[api.JobID]*PodGroupMoveAggregate{}}
	for _, move := range plan.CommittedMoves {
		moveAggregate.addMove(context, move)
	}
	for _, move := range plan.Moves {
		moveAggregate.addMove(context, move)
	}
	moveAggregate.AffectedPodGroups = int64(len(moveAggregate.ByPodGroup))
	plan.aggregate = moveAggregate
	return moveAggregate
}

func (moveAggregate *PlanMoveAggregate) addMove(context *PlanContext, move *Move) {
	if move == nil || move.Task == nil || move.To == move.From {
		return // not actually relocated
	}
	requestedResource := Scalar(move.Task.InitResreq, context.Resource())
	moveAggregate.MovedPods++
	moveAggregate.MovedResource += requestedResource
	podGroupAggregate := moveAggregate.ByPodGroup[move.Task.Job]
	if podGroupAggregate == nil {
		podGroupAggregate = &PodGroupMoveAggregate{}
		moveAggregate.ByPodGroup[move.Task.Job] = podGroupAggregate
	}
	podGroupAggregate.MovedPods++
	podGroupAggregate.MovedResource += requestedResource
}

// DisruptionCost is a flat summary of a plan's default dimensions, for
// status/report output — not the comparison mechanism.
type DisruptionCost struct {
	AffectedPodGroups int64
	MovedResource     int64
	MovedPods         int64
}

// CalculateDisruptionCost summarizes a move set's default dimensions for targetResource.
func CalculateDisruptionCost(moves []*Move, targetResource v1.ResourceName) DisruptionCost {
	plan := &CandidatePlan{Moves: moves}
	moveAggregate := plan.MoveAggregate(&PlanContext{TargetResource: targetResource})
	return DisruptionCost{
		AffectedPodGroups: moveAggregate.AffectedPodGroups,
		MovedResource:     moveAggregate.MovedResource,
		MovedPods:         moveAggregate.MovedPods,
	}
}
