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
	"volcano.sh/volcano/pkg/repackengine/api"
)

// Report is the search outcome rendered from a RepackPlan — the engine-side,
// CRD-independent shape projected into RepackRun.status.plan (§4.6).
type Report struct {
	NodesFreed             int   // realized node-level benefit (whole nodes freed)
	MovedResource          int64 // target-resource units relocated
	AffectedPodGroups      int64 // distinct gangs disrupted
	FragmentationRateDelta float64
	// FragmentationRateBefore/After are the cluster-wide fragmentation rate (0-1)
	// for the target resource before/after the plan, feeding
	// status.plan.summary.frag{Before,After}Percent. Scope limits eligible actions,
	// not this cluster health metric. after = before + FragmentationRateDelta
	// (freeing a node reduces the occupied-node count by one).
	FragmentationRateBefore float64
	FragmentationRateAfter  float64
}

// RenderReport turns a plan into the report. Nil plan → empty report.
func RenderReport(plan *api.RepackPlan) Report {
	if plan == nil {
		return Report{}
	}
	return Report{
		NodesFreed:              plan.NodesFreed(),
		MovedResource:           plan.Cost.MovedResource,
		AffectedPodGroups:       plan.Cost.AffectedPodGroups,
		FragmentationRateDelta:  plan.FragmentationRateDelta(),
		FragmentationRateBefore: plan.Before.FragmentationRate(),
		FragmentationRateAfter:  plan.Before.FragmentationRate() + plan.FragmentationRateDelta(),
	}
}
