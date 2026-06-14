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

// LogNetworkTopologyInitResourceCache logs HyperNode resource cache initialization cost.
func LogNetworkTopologyInitResourceCache(hyperNodeCount, nodeCount int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: init hyperNode resource cache",
		"hyperNodes", hyperNodeCount,
		"nodes", nodeCount,
		"latency", latency,
	)
}

// LogNetworkTopologyGradientForOwner logs job/subJob gradient callback cost.
func LogNetworkTopologyGradientForOwner(
	ownerType string,
	ownerRef any,
	rootHyperNode string,
	highestAllowedTier, eligibleTiers int,
	latency time.Duration,
) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: gradient for owner",
		"ownerType", ownerType,
		"owner", ownerRef,
		"rootHyperNode", rootHyperNode,
		"highestAllowedTier", highestAllowedTier,
		"eligibleTiers", eligibleTiers,
		"latency", latency,
	)
}

// NetworkTopologyBuildGradientStats holds BFS counters for topology gradient build.
type NetworkTopologyBuildGradientStats struct {
	RootHyperNode      string
	SearchRoot         string
	HighestAllowedTier int
	AllocatedHyperNode string
	VisitedHyperNodes  int
	EligibleHyperNodes int
	EligibleTiers      int
	Latency            time.Duration
}

// LogNetworkTopologyBuildGradient logs BFS gradient build cost.
func LogNetworkTopologyBuildGradient(stats NetworkTopologyBuildGradientStats) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: build gradient",
		"rootHyperNode", stats.RootHyperNode,
		"searchRoot", stats.SearchRoot,
		"highestAllowedTier", stats.HighestAllowedTier,
		"allocatedHyperNode", stats.AllocatedHyperNode,
		"visitedHyperNodes", stats.VisitedHyperNodes,
		"eligibleHyperNodes", stats.EligibleHyperNodes,
		"eligibleTiers", stats.EligibleTiers,
		"latency", stats.Latency,
	)
}

// LogNetworkTopologyHyperNodeOrder logs HyperNode ordering cost for a subJob.
func LogNetworkTopologyHyperNodeOrder(
	subJobUID string,
	candidateHyperNodes, tiedCandidates int,
	binpackLatency, taskNumLatency, totalLatency time.Duration,
) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: hyperNode order",
		"subJob", subJobUID,
		"candidateHyperNodes", candidateHyperNodes,
		"tiedCandidates", tiedCandidates,
		"binpackLatency", binpackLatency,
		"taskNumLatency", taskNumLatency,
		"latency", totalLatency,
	)
}

// LogNetworkTopologyBatchNodeOrder logs batch node order entry cost.
func LogNetworkTopologyBatchNodeOrder(taskUID, path string, nodeCount int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: batch node order",
		"task", taskUID,
		"path", path,
		"candidateNodes", nodeCount,
		"latency", latency,
	)
}

// LogNetworkTopologyBatchNodeOrderNormal logs normal-pod batch node order cost.
func LogNetworkTopologyBatchNodeOrderNormal(taskUID string, nodeCount, tierCount int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: batch node order normal pod",
		"task", taskUID,
		"candidateNodes", nodeCount,
		"tiers", tierCount,
		"latency", latency,
	)
}

// LogNetworkTopologyBatchNodeOrderNetworkAware logs network-aware batch node order cost.
func LogNetworkTopologyBatchNodeOrderNetworkAware(taskUID string, nodeCount, tiedCandidates int, latency time.Duration) {
	if !Enabled() {
		return
	}
	klog.V(Level).InfoS("network topology perf: batch node order network aware",
		"task", taskUID,
		"candidateNodes", nodeCount,
		"tiedCandidates", tiedCandidates,
		"latency", latency,
	)
}
