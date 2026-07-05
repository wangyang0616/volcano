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
// flagged inexact (so callers know FragRate may over-estimate).
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

func TestFragRateAndWeighted(t *testing.T) {
	per := map[v1.ResourceName]ResourceFrag{
		gpu: {Resource: gpu, M: 20, B: 18, A: 16},
		"huawei.com/Ascend910": {Resource: "huawei.com/Ascend910", M: 8, B: 5, A: 5},
	}
	if got := per[gpu].FragRate(); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("gpu FragRate=%v want 0.10", got)
	}
	// Weighted = Sum(B-A)/Sum(M) = (2+0)/(20+8)
	if got := WeightedFragRate(per); math.Abs(got-2.0/28.0) > 1e-9 {
		t.Errorf("WeightedFragRate=%v want %v", got, 2.0/28.0)
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
		node(8, 4, 4),       // 8-GPU node, 4 used by one 4-GPU task
		node(8, 4, 2, 1, 1), // fragmented: three small tasks summing 4
		node(8, 0),          // empty 8-GPU node
		{Allocatable: &api.Resource{ScalarResources: map[v1.ResourceName]float64{"cpu": 0}}}, // non-GPU node, ignored
	}
	f := MeasureResource(nodes, gpu)
	// M=3 GPU nodes; B=2 occupied; demand {4,2,1,1}=8 -> A=ceil(8/8)=1
	if f.M != 3 || f.B != 2 || f.A != 1 || !f.Exact {
		t.Fatalf("MeasureResource = %+v; want M=3 B=2 A=1 Exact=true", f)
	}
	if got := f.FragRate(); math.Abs(got-1.0/3.0) > 1e-9 { // (2-1)/3
		t.Errorf("FragRate=%v want %v", got, 1.0/3.0)
	}
}
