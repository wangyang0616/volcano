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
	"math"
	"math/rand"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

// ---- brute-force optimal node count (ground truth for small instances) ----

// minBins returns the minimum number of capacity-C bins to pack items
// (each item < C) via backtracking; this is the true optimum.
func minBins(items []int64, c int64) int64 {
	xs := append([]int64(nil), items...)
	sort.Slice(xs, func(i, j int) bool { return xs[i] > xs[j] })
	var sum int64
	for _, x := range xs {
		sum += x
	}
	canPack := func(k int64) bool {
		bins := make([]int64, k)
		var bt func(i int) bool
		bt = func(i int) bool {
			if i == len(xs) {
				return true
			}
			seen := map[int64]bool{}
			for b := range bins {
				if seen[bins[b]] { // symmetry pruning: equal-load bins once
					continue
				}
				seen[bins[b]] = true
				if bins[b]+xs[i] <= c {
					bins[b] += xs[i]
					if bt(i + 1) {
						return true
					}
					bins[b] -= xs[i]
				}
			}
			return false
		}
		return bt(0)
	}
	for k := ceilDiv(sum, c); ; k++ {
		if canPack(k) {
			return k
		}
	}
}

func bruteOptimalNodes(requests []int64, c int64) int64 {
	var big int64
	small := make([]int64, 0)
	for _, g := range requests {
		if g >= c {
			big += ceilDiv(g, c)
		} else if g > 0 {
			small = append(small, g)
		}
	}
	return big + minBins(small, c)
}

// ---------------------------------------------------------------------------

func TestOptimalNodes_DesignExamples(t *testing.T) {
	cases := []struct {
		reqs []int64
		c    int64
		want int64
		desc string
	}{
		{[]int64{4, 4, 2, 1, 1}, 8, 2, "perfect pack 4+4 / 2+1+1"},
		{[]int64{8, 8, 8}, 8, 3, "whole-node tasks"},
		{[]int64{4, 4, 4}, 8, 2, "4+4 / 4"},
		{[]int64{16, 4}, 8, 3, "16 spans 2 nodes + one 4"},
		{[]int64{16, 16, 4}, 8, 5, "two 16s (2 each) + one 4"},
		{[]int64{2, 2, 2, 2, 1}, 8, 2, "2*4 + 1"},
		{[]int64{1, 1, 1, 1, 1, 1, 1, 1, 1}, 8, 2, "nine 1s"},
	}
	for _, tc := range cases {
		a, exact := OptimalNodes(tc.reqs, tc.c)
		if a != tc.want || !exact {
			t.Errorf("%s: OptimalNodes(%v,%d)=(%d,%v), want (%d,true)", tc.desc, tc.reqs, tc.c, a, exact, tc.want)
		}
		if b := bruteOptimalNodes(tc.reqs, tc.c); b != a {
			t.Errorf("%s: closed=%d != brute=%d", tc.desc, a, b)
		}
	}
}

// Core proposition: powers-of-2 => closed form equals the true optimum.
func TestOptimalNodes_MatchesBruteForce_Pow2(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	pow2 := []int64{1, 2, 4, 8, 16, 32}
	for i := 0; i < 5000; i++ {
		c := []int64{4, 8, 16}[r.Intn(3)]
		choices := make([]int64, 0)
		for _, p := range pow2 {
			if p <= 4*c {
				choices = append(choices, p)
			}
		}
		n := r.Intn(12) + 1
		reqs := make([]int64, n)
		for j := range reqs {
			reqs[j] = choices[r.Intn(len(choices))]
		}
		a, exact := OptimalNodes(reqs, c)
		if !exact {
			t.Fatalf("pow2 input flagged inexact: reqs=%v c=%d", reqs, c)
		}
		if b := bruteOptimalNodes(reqs, c); a != b {
			t.Fatalf("closed=%d != brute=%d for reqs=%v c=%d", a, b, reqs, c)
		}
	}
}

// Non-powers-of-2: closed form is a valid lower bound (<= optimum) and is
// flagged inexact (so callers know FragmentationRate may over-estimate).
func TestOptimalNodes_LowerBound_NonPow2(t *testing.T) {
	if a, exact := OptimalNodes([]int64{5, 5, 5}, 8); a != 2 || exact {
		t.Errorf("{5,5,5}/8: got (%d,%v), want (2,false) — loose lower bound", a, exact)
	}
	r := rand.New(rand.NewSource(11))
	for i := 0; i < 1000; i++ {
		c := []int64{6, 8, 10}[r.Intn(3)]
		n := r.Intn(8) + 1
		reqs := make([]int64, n)
		for j := range reqs {
			reqs[j] = int64(r.Intn(int(c-1)) + 1)
		}
		a, _ := OptimalNodes(reqs, c)
		if b := bruteOptimalNodes(reqs, c); a > b {
			t.Fatalf("closed=%d exceeds optimum=%d for reqs=%v c=%d (not a lower bound!)", a, b, reqs, c)
		}
	}
}

func TestFragmentationRate(t *testing.T) {
	fragmentation := ResourceFragmentation{
		Resource: gpu, ProvidingNodeCount: 20, OccupiedNodeCount: 18, OptimalOccupiedNodeCount: 16,
	}
	if got := fragmentation.FragmentationRate(); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("gpu FragmentationRate=%v want 0.10", got)
	}
}

func TestMeasureResource(t *testing.T) {
	mkRes := func(n int64) *api.Resource {
		return &api.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
	}
	mkTask := func(g int64) *api.TaskInfo { return &api.TaskInfo{Resreq: mkRes(g)} }
	node := func(cap, used int64, reqs ...int64) *api.NodeInfo {
		tasks := map[api.TaskID]*api.TaskInfo{}
		for i, g := range reqs {
			tasks[api.TaskID(string(rune('a'+i)))] = mkTask(g)
		}
		return &api.NodeInfo{Allocatable: mkRes(cap), Used: mkRes(used), Tasks: tasks}
	}
	nodes := []*api.NodeInfo{
		node(8, 4, 4),       // 8-TargetResource node, 4 used by one 4-TargetResource task
		node(8, 4, 2, 1, 1), // fragmented: three small tasks summing 4
		node(8, 0),          // empty 8-TargetResource node
		{Allocatable: &api.Resource{ScalarResources: map[v1.ResourceName]float64{"cpu": 0}}}, // non-TargetResource node, ignored
	}
	f := MeasureResourceFragmentation(nodes, gpu)
	// Three nodes provide the target resource; two are occupied; demand {4,2,1,1}=8
	// means the optimal occupied-node count is ceil(8/8)=1.
	if f.ProvidingNodeCount != 3 || f.OccupiedNodeCount != 2 || f.OptimalOccupiedNodeCount != 1 || !f.Exact {
		t.Fatalf("MeasureResourceFragmentation = %+v; want providing=3 occupied=2 optimal=1 Exact=true", f)
	}
	if got := f.FragmentationRate(); math.Abs(got-1.0/3.0) > 1e-9 { // (2-1)/3
		t.Errorf("FragmentationRate=%v want %v", got, 1.0/3.0)
	}
}

func TestMeasureResourceHeterogeneousIsOrderIndependentAndBounded(t *testing.T) {
	mkRes := func(n int64) *api.Resource {
		return &api.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
	}
	node := func(name string, cap, used, req int64) *api.NodeInfo {
		tasks := map[api.TaskID]*api.TaskInfo{}
		if req > 0 {
			tasks[api.TaskID(name+"-pod")] = &api.TaskInfo{Resreq: mkRes(req)}
		}
		return &api.NodeInfo{Name: name, Allocatable: mkRes(cap), Used: mkRes(used), Tasks: tasks}
	}
	four := node("four", 4, 4, 4)
	eight := node("eight", 8, 8, 8)
	emptyEight := node("empty-eight", 8, 0, 0)

	a := MeasureResourceFragmentation([]*api.NodeInfo{four, eight, emptyEight}, gpu)
	b := MeasureResourceFragmentation([]*api.NodeInfo{emptyEight, eight, four}, gpu)
	if a.OptimalOccupiedNodeCount != b.OptimalOccupiedNodeCount || a.OccupiedNodeCount != b.OccupiedNodeCount || a.ProvidingNodeCount != b.ProvidingNodeCount {
		t.Fatalf("heterogeneous metric depends on node order: first=%+v reversed=%+v", a, b)
	}
	if a.Exact || a.OptimalOccupiedNodeCount < 0 || a.OptimalOccupiedNodeCount > a.OccupiedNodeCount || a.FragmentationRate() < 0 || a.FragmentationRate() > 1 {
		t.Fatalf("heterogeneous metric must be inexact and bounded: %+v rate=%v", a, a.FragmentationRate())
	}
}
