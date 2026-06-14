/*
Copyright 2025 The Volcano Authors.

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

package perflog

import (
	"time"

	"k8s.io/klog/v2"
)

// PodGroupAntiAffinityJobContext identifies a job in podGroup anti-affinity perf logs.
type PodGroupAntiAffinityJobContext struct {
	Namespace string
	Name      string
	Pods      string
}

// PodGroupAntiAffinityScanStats holds counters from MatchingPodGroupsAllocatedHyperNodesForTerm.
type PodGroupAntiAffinityScanStats struct {
	TermTier           int
	JobsTotal          int
	JobsMatched        int
	JobsWithPlacement  int
	InferredPlacements int
	OccupiedHyperNodes int
	Latency            time.Duration
}

// CollectMatchingStats aggregates occupancy across required anti-affinity terms.
type CollectMatchingStats struct {
	TotalOccupiedHyperNodes int
}

// BFSAntiAffinityStats aggregates HyperNode BFS traversal counters.
type BFSAntiAffinityStats struct {
	VisitedHyperNodes  int
	EligibleHyperNodes int
}

// LogPodGroupAntiAffinityScanMatchingJobs logs job scan cost for one affinity term.
func LogPodGroupAntiAffinityScanMatchingJobs(selfJobNamespace, selfJobName string, stats PodGroupAntiAffinityScanStats) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("podGroup anti-affinity perf: scan matching jobs",
		"selfJob", klog.KRef(selfJobNamespace, selfJobName),
		"termTier", stats.TermTier,
		"jobsTotal", stats.JobsTotal,
		"jobsMatched", stats.JobsMatched,
		"jobsWithPlacement", stats.JobsWithPlacement,
		"inferredPlacements", stats.InferredPlacements,
		"occupiedHyperNodes", stats.OccupiedHyperNodes,
		"latency", stats.Latency,
	)
}

// LogPodGroupAntiAffinityGradientForJob logs end-to-end hard gradient evaluation for a job.
func LogPodGroupAntiAffinityGradientForJob(job PodGroupAntiAffinityJobContext, termCount, tierCount int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("podGroup anti-affinity perf: gradient for job",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", job.Pods,
		"terms", termCount,
		"eligibleTiers", tierCount,
		"latency", latency,
	)
}

// LogPodGroupAntiAffinityBuildGradient logs phased hard gradient build cost.
func LogPodGroupAntiAffinityBuildGradient(
	job PodGroupAntiAffinityJobContext,
	collectStats CollectMatchingStats,
	collectLatency time.Duration,
	bfsStats BFSAntiAffinityStats,
	bfsLatency, totalLatency time.Duration,
) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("podGroup anti-affinity perf: build gradient",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", job.Pods,
		"occupiedHyperNodes", collectStats.TotalOccupiedHyperNodes,
		"collectLatency", collectLatency,
		"visitedHyperNodes", bfsStats.VisitedHyperNodes,
		"eligibleHyperNodes", bfsStats.EligibleHyperNodes,
		"bfsLatency", bfsLatency,
		"latency", totalLatency,
	)
}

// LogPodGroupAntiAffinityPreferredOrder logs soft anti-affinity HyperNode ordering cost.
func LogPodGroupAntiAffinityPreferredOrder(
	job PodGroupAntiAffinityJobContext,
	termCount, candidateCount, occupiedHyperNodes int,
	collectLatency, totalLatency time.Duration,
) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("podGroup anti-affinity perf: preferred order",
		"job", klog.KRef(job.Namespace, job.Name),
		"pods", job.Pods,
		"terms", termCount,
		"candidateHyperNodes", candidateCount,
		"occupiedHyperNodes", occupiedHyperNodes,
		"collectLatency", collectLatency,
		"latency", totalLatency,
	)
}
