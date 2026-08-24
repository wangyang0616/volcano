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

// Package api implements the fragmentation index and pure model/contracts for
// the repack (defragmentation) engine. See docs/design/repack-design.md §6.4.
//
// Per accelerator resource R (e.g. nvidia.com/gpu, huawei.com/Ascend910):
//
//	FragmentationRate(R) = (occupied nodes - optimal occupied nodes) / providing nodes
//	  providing nodes = nodes with Allocatable[R] > 0
//	  occupied nodes = providing nodes with Used[R] > 0
//	  optimal occupied nodes = theoretical minimum for R's demand (see OptimalNodes)
package api

import (
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// ResourceFragmentation is the fragmentation measurement for one accelerator resource.
type ResourceFragmentation struct {
	Resource                 v1.ResourceName
	ProvidingNodeCount       int64 // nodes providing this resource (in scope)
	OccupiedNodeCount        int64 // nodes currently occupied by this resource
	OptimalOccupiedNodeCount int64 // theoretical-optimal occupied nodes for the demand
	// Exact is true when OptimalOccupiedNodeCount is computed exactly (powers-of-2 requests on a
	// power-of-2, homogeneous node capacity, design §4.12.2a). When false,
	// OptimalOccupiedNodeCount is a constraint-aware lower bound and FragmentationRate may over-estimate.
	Exact bool
}

// FragmentationRate returns (occupiedNodes-optimalNodes)/providingNodes.
func (fragmentation ResourceFragmentation) FragmentationRate() float64 {
	if fragmentation.ProvidingNodeCount == 0 {
		return 0
	}
	return float64(fragmentation.OccupiedNodeCount-fragmentation.OptimalOccupiedNodeCount) / float64(fragmentation.ProvidingNodeCount)
}

// OptimalNodes returns the minimum number of nodes (each of the given capacity)
// needed to host all requests, plus whether the result is exact.
//
// Model (design §4.12.2a): a request g >= capacity is a multi-node task that
// occupies ceil(g/capacity) whole nodes; requests < capacity are packed into
// shared nodes. Under the product constraints C1/C2 (all requests and the node
// capacity are powers of two) the divisible-chain property makes the volume
// bound tight, so the closed form below is exactly the optimum.
//
// Validated against a brute-force optimal bin-packer: 5000 random powers-of-2
// instances all matched (frag_validate.py / TestOptimalNodes_MatchesBruteForce).
func OptimalNodes(resourceRequests []int64, nodeCapacity int64) (optimalNodeCount int64, exact bool) {
	if nodeCapacity <= 0 {
		return 0, false
	}
	var wholeNodeDemand, sharedNodeDemand int64
	exact = isPowerOfTwo(nodeCapacity)
	for _, requestedResource := range resourceRequests {
		if requestedResource <= 0 {
			continue
		}
		if !isPowerOfTwo(requestedResource) {
			exact = false
		}
		if requestedResource >= nodeCapacity {
			wholeNodeDemand += ceilDiv(requestedResource, nodeCapacity) // whole nodes for multi-node tasks
		} else {
			sharedNodeDemand += requestedResource // sub-node tasks share via volume packing
		}
	}
	return wholeNodeDemand + ceilDiv(sharedNodeDemand, nodeCapacity), exact
}

// MeasureResourceFragmentation computes the fragmentation of a single accelerator resource
// over the given nodes (already restricted to the run's scope by the caller).
// Demand is taken from the tasks currently placed on the resource-providing
// nodes. When node capacities for the resource are not homogeneous, the optimal
// occupied-node count falls
// back to a volume lower bound and Exact is false.
func MeasureResourceFragmentation(nodes []*api.NodeInfo, targetResource v1.ResourceName) ResourceFragmentation {
	fragmentation := ResourceFragmentation{Resource: targetResource}

	var nodeCapacity int64
	homogeneous := true
	nodeCapacities := make([]int64, 0, len(nodes))
	resourceRequests := make([]int64, 0, 64)

	for _, node := range nodes {
		if node == nil || node.Allocatable == nil {
			continue
		}
		capacity := Scalar(node.Allocatable, targetResource)
		if capacity <= 0 {
			continue // node does not provide this resource
		}
		fragmentation.ProvidingNodeCount++
		nodeCapacities = append(nodeCapacities, capacity)
		if nodeCapacity == 0 {
			nodeCapacity = capacity
		} else if capacity != nodeCapacity {
			homogeneous = false
		}
		resourceUsage := int64(0)
		if node.Used != nil {
			resourceUsage = Scalar(node.Used, targetResource)
		}
		if resourceUsage > 0 {
			fragmentation.OccupiedNodeCount++
		}
		klog.V(5).InfoS("repack frag: node accelerator usage", "node", node.Name,
			"resource", targetResource, "capacity", capacity, "used", resourceUsage)
		for _, task := range node.Tasks {
			if task == nil || task.Resreq == nil {
				continue
			}
			if requestedResource := Scalar(task.Resreq, targetResource); requestedResource > 0 {
				resourceRequests = append(resourceRequests, requestedResource)
			}
		}
	}

	if fragmentation.ProvidingNodeCount == 0 || nodeCapacity == 0 {
		klog.V(5).InfoS("repack frag: no node provides this resource", "resource", targetResource)
		return fragmentation
	}
	if homogeneous {
		fragmentation.OptimalOccupiedNodeCount, fragmentation.Exact = OptimalNodes(resourceRequests, nodeCapacity)
	} else {
		// Heterogeneous pools cannot be evaluated with an arbitrary first-node
		// capacity: that made the metric depend on map iteration order and could
		// even produce an optimal count above the occupied count. Use the minimum
		// number of largest real nodes whose
		// aggregate capacity covers demand. This is a deterministic lower bound;
		// exact bin packing remains intentionally out of the hot measurement path.
		sort.Slice(nodeCapacities, func(i, j int) bool { return nodeCapacities[i] > nodeCapacities[j] })
		var totalResourceDemand, coveredCapacity int64
		for _, requestedResource := range resourceRequests {
			totalResourceDemand += requestedResource
		}
		for _, capacity := range nodeCapacities {
			if coveredCapacity >= totalResourceDemand {
				break
			}
			coveredCapacity += capacity
			fragmentation.OptimalOccupiedNodeCount++
		}
		fragmentation.Exact = false
	}
	// The current placement itself proves an optimum cannot require more than the
	// current number of occupied nodes. Clamp defensive lower-bound approximations and stale cache
	// combinations so FragmentationRate always remains in its documented [0,1] range.
	if fragmentation.OptimalOccupiedNodeCount > fragmentation.OccupiedNodeCount {
		fragmentation.OptimalOccupiedNodeCount = fragmentation.OccupiedNodeCount
	}
	return fragmentation
}

// scalar reads an accelerator (scalar) resource amount as a rounded int64.
// Accelerator cards are whole units; CPU/memory would keep Quantity.
func Scalar(resource *api.Resource, resourceName v1.ResourceName) int64 {
	if resource == nil || resource.ScalarResources == nil {
		return 0
	}
	// Volcano stores scalar/extended resources in MILLI-units (1 device = 1000),
	// see scheduler/api NewResource. This returns that raw milli value, rounded;
	// it is the internal unit the fragmentation math and drain budget both use.
	// For a human/user-facing whole-device count use Cards.
	return int64(resource.ScalarResources[resourceName] + 0.5)
}

// Cards returns the whole number of accelerator devices in r for the given
// resource (Scalar / 1000). This is the unit users deal in — status.plan card
// counts and spec.maxPerRun.resources — as opposed to the internal milli Scalar.
func Cards(resource *api.Resource, resourceName v1.ResourceName) int64 {
	return Scalar(resource, resourceName) / 1000
}

func ceilDiv(numerator, denominator int64) int64 {
	if denominator <= 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}

func isPowerOfTwo(value int64) bool { return value > 0 && (value&(value-1)) == 0 }
