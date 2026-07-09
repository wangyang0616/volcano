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
// the repack (defragmentation) engine. See docs/design/repack-policy-design.md §4.12.
//
// Per accelerator resource R (e.g. nvidia.com/gpu, huawei.com/Ascend910):
//
//	FragRate(R) = (B_R - A_R) / M_R
//	  M_R = number of nodes providing R (Allocatable[R] > 0)
//	  B_R = number of those nodes currently occupied (Used[R] > 0)
//	  A_R = theoretical-optimal occupied nodes for R's demand (see OptimalNodes)
//
// The cluster KPI WeightedFragRate aggregates per-resource rates weighted by
// node count M_R; since a node provides exactly one accelerator type the
// node-count weighting collapses to Sum(B_R-A_R) / Sum(M_R).
package api

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// ResourceFrag is the fragmentation measurement for one accelerator resource.
type ResourceFrag struct {
	Resource v1.ResourceName
	M        int64 // nodes providing this resource (in scope)
	B        int64 // nodes currently occupied by this resource
	A        int64 // theoretical-optimal occupied nodes for the demand
	// Exact is true when A is computed exactly (powers-of-2 requests on a
	// power-of-2, homogeneous node capacity, design §4.12.2a). When false,
	// A is a constraint-aware lower bound and FragRate may over-estimate.
	Exact bool
}

// FragRate returns (B-A)/M; 0 when M==0.
func (f ResourceFrag) FragRate() float64 {
	if f.M == 0 {
		return 0
	}
	return float64(f.B-f.A) / float64(f.M)
}

// OptimalNodes returns A: the minimum number of nodes (each of the given
// capacity) needed to host all requests, plus whether the result is exact.
//
// Model (design §4.12.2a): a request g >= capacity is a multi-node task that
// occupies ceil(g/capacity) whole nodes; requests < capacity are packed into
// shared nodes. Under the product constraints C1/C2 (all requests and the node
// capacity are powers of two) the divisible-chain property makes the volume
// bound tight, so the closed form below is exactly the optimum.
//
// Validated against a brute-force optimal bin-packer: 5000 random powers-of-2
// instances all matched (frag_validate.py / TestOptimalNodes_MatchesBruteForce).
func OptimalNodes(requests []int64, capacity int64) (a int64, exact bool) {
	if capacity <= 0 {
		return 0, false
	}
	var big, small int64
	exact = isPow2(capacity)
	for _, g := range requests {
		if g <= 0 {
			continue
		}
		if !isPow2(g) {
			exact = false
		}
		if g >= capacity {
			big += ceilDiv(g, capacity) // whole nodes for multi-node tasks
		} else {
			small += g // sub-node tasks share via volume packing
		}
	}
	return big + ceilDiv(small, capacity), exact
}

// MeasureResource computes the fragmentation of a single accelerator resource
// over the given nodes (already restricted to the run's scope by the caller).
// Demand is taken from the tasks currently placed on the resource-providing
// nodes. When node capacities for the resource are not homogeneous, A falls
// back to a volume lower bound and Exact is false.
func MeasureResource(nodes []*api.NodeInfo, resource v1.ResourceName) ResourceFrag {
	out := ResourceFrag{Resource: resource}

	var capacity int64
	homogeneous := true
	requests := make([]int64, 0, 64)

	for _, node := range nodes {
		if node == nil || node.Allocatable == nil {
			continue
		}
		cap := Scalar(node.Allocatable, resource)
		if cap <= 0 {
			continue // node does not provide this resource
		}
		out.M++
		if capacity == 0 {
			capacity = cap
		} else if cap != capacity {
			homogeneous = false
		}
		used := int64(0)
		if node.Used != nil {
			used = Scalar(node.Used, resource)
		}
		if used > 0 {
			out.B++
		}
		klog.V(5).InfoS("repack frag: node accelerator usage", "node", node.Name,
			"resource", resource, "capacity", cap, "used", used)
		for _, task := range node.Tasks {
			if task == nil || task.Resreq == nil {
				continue
			}
			if g := Scalar(task.Resreq, resource); g > 0 {
				requests = append(requests, g)
			}
		}
	}

	if out.M == 0 || capacity == 0 {
		klog.V(5).InfoS("repack frag: no node provides this resource (M=0)", "resource", resource)
		return out
	}
	out.A, out.Exact = OptimalNodes(requests, capacity)
	if !homogeneous {
		out.Exact = false // mixed-capacity pools: A is only a lower bound (P1)
	}
	return out
}

// WeightedFragRate aggregates per-resource fragmentation into the cluster KPI
// using the default node-count weighting, which collapses to
// Sum(B-A)/Sum(M) because accelerator nodes are disjoint per resource
// (design §4.6.2 / §4.16 FragWeightFn). Returns 0 when no resource nodes exist.
func WeightedFragRate(per map[v1.ResourceName]ResourceFrag) float64 {
	var num, den int64
	for _, f := range per {
		num += f.B - f.A
		den += f.M
	}
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// scalar reads an accelerator (scalar) resource amount as a rounded int64.
// Accelerator cards are whole units; CPU/memory (P1) would keep Quantity.
func Scalar(r *api.Resource, name v1.ResourceName) int64 {
	if r == nil || r.ScalarResources == nil {
		return 0
	}
	// Volcano stores scalar/extended resources in MILLI-units (1 device = 1000),
	// see scheduler/api NewResource. This returns that raw milli value, rounded;
	// it is the internal unit the fragmentation math and drain budget both use.
	// For a human/user-facing whole-device count use Cards.
	return int64(r.ScalarResources[name] + 0.5)
}

// Cards returns the whole number of accelerator devices in r for the given
// resource (Scalar / 1000). This is the unit users deal in — status.plan card
// counts and spec.maxPerRun.resources — as opposed to the internal milli Scalar.
func Cards(r *api.Resource, name v1.ResourceName) int64 {
	return Scalar(r, name) / 1000
}

func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func isPow2(x int64) bool { return x > 0 && (x&(x-1)) == 0 }
