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

package framework

import (
	"sort"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Report is the DryRun payload rendered from a RepackPlan — the engine-side,
// CRD-independent shape that populates RepackRun.status.report (§4.6).
type Report struct {
	RecommendedPodGroups []string // affected gangs (PodGroup ids), sorted distinct
	RecommendedNodes     []string // nodes that would be freed, sorted
	NodesFreed           int      // realized node-level benefit
	Benefit              float64  // realized weighted benefit (node + hypernode units)
	MovedPods            int64
	MovedResource        int64
	AffectedPodGroups    int64
	FragRateDelta        float64
	// FragRateBefore/After are the resource fragmentation rate (0-1) before/after
	// the plan, feeding status.plan.summary.frag{Before,After}Percent. after =
	// before + FragRateDelta (freeing a node drops B by 1: (B-A-freed)/M).
	FragRateBefore float64
	FragRateAfter  float64
}

// RenderReport turns a plan into the DryRun report. Nil plan → empty report.
func RenderReport(plan *api.RepackPlan) Report {
	if plan == nil {
		return Report{}
	}
	pgs := plan.AffectedPodGroups()
	rec := make([]string, 0, len(pgs))
	for _, j := range pgs {
		rec = append(rec, string(j))
	}
	nodes := append([]string(nil), plan.FreedNodes...)
	sort.Strings(nodes)
	return Report{
		RecommendedPodGroups: rec,
		RecommendedNodes:     nodes,
		NodesFreed:           plan.NodesFreed(),
		Benefit:              plan.Benefit(),
		MovedPods:            plan.Cost.MovedPods,
		MovedResource:        plan.Cost.MovedGPU,
		AffectedPodGroups:    plan.Cost.AffectedPodGroups,
		FragRateDelta:        plan.FragRateDelta(),
		FragRateBefore:       plan.Before.FragRate(),
		FragRateAfter:        plan.Before.FragRate() + plan.FragRateDelta(),
	}
}
