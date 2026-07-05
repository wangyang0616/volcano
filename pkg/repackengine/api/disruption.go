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

// PlanContext carries lookups shared by every scoring strategy in a comparison.
// It is built by the engine Session and passed to the disruption score functions
// that plugins register.
type PlanContext struct {
	GPU      v1.ResourceName              // accelerator resource being defragmented
	PodGroup func(api.JobID) PodGroupView // per-PodGroup facts; nil-safe via zero value
}

// View returns the PodGroup facts, zero-valued when unknown.
func (c *PlanContext) View(id api.JobID) PodGroupView {
	if c == nil || c.PodGroup == nil {
		return PodGroupView{}
	}
	return c.PodGroup(id)
}

// Resource returns the target accelerator, defaulting to nvidia.com/gpu.
func (c *PlanContext) Resource() v1.ResourceName {
	if c == nil || c.GPU == "" {
		return v1.ResourceName("nvidia.com/gpu")
	}
	return c.GPU
}

// CandidatePlan is one rearrangement under comparison: the moves it implies.
// Aggregates are computed once and cached.
type CandidatePlan struct {
	Moves []*Move
	agg   *PlanAgg
}

// PGAgg is the per-PodGroup move aggregate.
type PGAgg struct {
	MovedPods int64
	MovedGPU  int64
}

// PlanAgg is the whole-plan move aggregate, with a per-PodGroup breakdown.
type PlanAgg struct {
	AffectedPGs int64
	MovedGPU    int64
	MovedPods   int64
	ByPG        map[api.JobID]*PGAgg
}

// Aggregate computes (and caches) the move aggregate for the given context.
// Exported so disruption score functions in plugin packages can build on it.
func (p *CandidatePlan) Aggregate(ctx *PlanContext) *PlanAgg {
	if p.agg != nil {
		return p.agg
	}
	a := &PlanAgg{ByPG: map[api.JobID]*PGAgg{}}
	for _, m := range p.Moves {
		if m == nil || m.Task == nil || m.To == m.From {
			continue // not actually relocated
		}
		g := Scalar(m.Task.InitResreq, ctx.Resource())
		a.MovedPods++
		a.MovedGPU += g
		pg := a.ByPG[m.Task.Job]
		if pg == nil {
			pg = &PGAgg{}
			a.ByPG[m.Task.Job] = pg
		}
		pg.MovedPods++
		pg.MovedGPU += g
	}
	a.AffectedPGs = int64(len(a.ByPG))
	p.agg = a
	return a
}

// DisruptionCost is a flat summary of a plan's default dimensions, for
// status/report output — not the comparison mechanism.
type DisruptionCost struct {
	AffectedPodGroups int64
	MovedGPU          int64
	MovedPods         int64
}

// CostOf summarizes a move set's default dimensions for resource gpuRes.
func CostOf(moves []*Move, gpuRes v1.ResourceName) DisruptionCost {
	p := &CandidatePlan{Moves: moves}
	a := p.Aggregate(&PlanContext{GPU: gpuRes})
	return DisruptionCost{
		AffectedPodGroups: a.AffectedPGs,
		MovedGPU:          a.MovedGPU,
		MovedPods:         a.MovedPods,
	}
}
