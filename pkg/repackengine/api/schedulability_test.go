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
	"fmt"
	"math/rand"
	"testing"

	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// gpuRes builds a single-resource (GPU) request/capacity.
func gpuRes(n int64) *api.Resource {
	return &api.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

// gpuTask builds a task named t<idx> currently on `from`, requesting g GPUs.
func gpuTask(idx int, from string, g int64) *api.TaskInfo {
	t := &api.TaskInfo{Name: fmt.Sprintf("t%d", idx), InitResreq: gpuRes(g)}
	t.NodeName = from
	return t
}

// buildDomain builds a GPU-only domain from per-node capacities, with an
// optional Fit allow-map keyed taskName -> nodeName -> allowed.
func buildDomain(caps map[string]int64, allow map[string]map[string]bool) ([]*api.NodeInfo, *Domain) {
	nodes := make([]*api.NodeInfo, 0, len(caps))
	// deterministic node order n0,n1,... for reproducible tests
	for i := 0; i < len(caps); i++ {
		name := fmt.Sprintf("n%d", i)
		if _, ok := caps[name]; ok {
			nodes = append(nodes, &api.NodeInfo{Name: name})
		}
	}
	free := func(n *api.NodeInfo) *api.Resource { return gpuRes(caps[n.Name]) }
	var fit Fit
	if allow != nil {
		fit = func(t *api.TaskInfo, n *api.NodeInfo) bool {
			byNode, ok := allow[t.Name]
			if !ok {
				return true
			}
			return byNode[n.Name]
		}
	}
	return nodes, NewDomain(nodes, free, fit)
}

// brute-force: does a complete assignment of reqs onto caps exist?
func bruteFeasible(reqs []int64, caps []int64, allowed func(ti, nj int) bool) bool {
	rem := append([]int64(nil), caps...)
	var bt func(i int) bool
	bt = func(i int) bool {
		if i == len(reqs) {
			return true
		}
		for j := range rem {
			if allowed != nil && !allowed(i, j) {
				continue
			}
			if reqs[i] <= rem[j] {
				rem[j] -= reqs[i]
				if bt(i + 1) {
					return true
				}
				rem[j] += reqs[i]
			}
		}
		return false
	}
	return bt(0)
}

// checkMoves verifies every task got a destination and no node is oversubscribed.
func checkMoves(t *testing.T, place []*api.TaskInfo, caps map[string]int64, moves []*Move) {
	t.Helper()
	if len(moves) != len(place) {
		t.Fatalf("moves=%d, want one per task (%d)", len(moves), len(place))
	}
	used := map[string]int64{}
	seen := map[string]bool{}
	for _, m := range moves {
		seen[m.Task.Name] = true
		used[m.To] += int64(m.Task.InitResreq.ScalarResources[gpu] + 0.5)
	}
	for node, u := range used {
		if u > caps[node] {
			t.Errorf("node %s oversubscribed: used=%d cap=%d", node, u, caps[node])
		}
	}
	for _, p := range place {
		if !seen[p.Name] {
			t.Errorf("task %s was not placed", p.Name)
		}
	}
}

func TestFeasible_Basic(t *testing.T) {
	caps := map[string]int64{"n0": 8, "n1": 8}
	_, d := buildDomain(caps, nil)
	place := []*api.TaskInfo{gpuTask(0, "", 8), gpuTask(1, "", 4), gpuTask(2, "", 4)}
	moves, ok := d.Feasible(place)
	if !ok {
		t.Fatal("expected feasible: {8,4,4} onto {8,8}")
	}
	checkMoves(t, place, caps, moves)
}

func TestFeasible_InfeasibleByCapacity(t *testing.T) {
	caps := map[string]int64{"n0": 8, "n1": 8}
	_, d := buildDomain(caps, nil)
	// demand 20 > capacity 16
	place := []*api.TaskInfo{gpuTask(0, "", 8), gpuTask(1, "", 8), gpuTask(2, "", 4)}
	if _, ok := d.Feasible(place); ok {
		t.Fatal("expected infeasible: demand 20 > capacity 16")
	}
}

func TestFeasible_FitExcludesNode(t *testing.T) {
	caps := map[string]int64{"n0": 8, "n1": 8}
	// t0 may only land on n1 (e.g. nodeSelector / taint); it needs a whole node,
	// so t1 (also 8) is forced onto n0 — still feasible.
	allow := map[string]map[string]bool{"t0": {"n0": false, "n1": true}}
	_, d := buildDomain(caps, allow)
	place := []*api.TaskInfo{gpuTask(0, "", 8), gpuTask(1, "", 8)}
	moves, ok := d.Feasible(place)
	if !ok {
		t.Fatal("expected feasible with t0 pinned to n1")
	}
	for _, m := range moves {
		if m.Task.Name == "t0" && m.To != "n1" {
			t.Errorf("t0 placed on %s, must be n1", m.To)
		}
	}

	// Now pin both 8-GPU tasks to n1: impossible (n1 holds only one).
	allow2 := map[string]map[string]bool{
		"t0": {"n0": false, "n1": true},
		"t1": {"n0": false, "n1": true},
	}
	_, d2 := buildDomain(caps, allow2)
	if _, ok := d2.Feasible(place); ok {
		t.Fatal("expected infeasible: two whole-node tasks both pinned to one node")
	}
}

// Backtracking is required: greedy first-fit that puts the two 3s on n0 (8)
// then a 5 fails, but 5+3 / 5+3 packs perfectly. The complete search must find it.
func TestFeasible_RequiresBacktracking(t *testing.T) {
	caps := map[string]int64{"n0": 8, "n1": 8}
	_, d := buildDomain(caps, nil)
	place := []*api.TaskInfo{gpuTask(0, "", 5), gpuTask(1, "", 5), gpuTask(2, "", 3), gpuTask(3, "", 3)}
	moves, ok := d.Feasible(place)
	if !ok {
		t.Fatal("expected feasible: {5,5,3,3} onto {8,8} (needs 5+3 per node)")
	}
	checkMoves(t, place, caps, moves)
}

func TestFeasible_MultiDimensional(t *testing.T) {
	// cpu+mem+gpu; node has 16 cpu / 64Gi / 8 gpu free.
	node := &api.NodeInfo{Name: "n0"}
	free := func(*api.NodeInfo) *api.Resource {
		return &api.Resource{MilliCPU: 16000, Memory: 64 << 30, ScalarResources: map[v1.ResourceName]float64{gpu: 8}}
	}
	d := NewDomain([]*api.NodeInfo{node}, free, nil)
	mk := func(name string, cpu, memGi, g int64) *api.TaskInfo {
		return &api.TaskInfo{Name: name, InitResreq: &api.Resource{
			MilliCPU: float64(cpu * 1000), Memory: float64(memGi << 30),
			ScalarResources: map[v1.ResourceName]float64{gpu: float64(g)},
		}}
	}
	// 4+4 gpu fits, 8+8=16 cpu fits, 32+32=64 mem fits — exactly full.
	place := []*api.TaskInfo{mk("a", 8, 32, 4), mk("b", 8, 32, 4)}
	if _, ok := d.Feasible(place); !ok {
		t.Fatal("expected feasible: two pods exactly filling one node")
	}
	// Bump gpu past capacity -> infeasible despite cpu/mem fitting.
	d2 := NewDomain([]*api.NodeInfo{node}, free, nil)
	place2 := []*api.TaskInfo{mk("a", 8, 32, 4), mk("b", 8, 32, 8)}
	if _, ok := d2.Feasible(place2); ok {
		t.Fatal("expected infeasible: gpu 4+8 > 8")
	}
}

// The solver is complete, so it must agree with brute force on every instance.
func TestFeasible_MatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for iter := 0; iter < 5000; iter++ {
		nNodes := r.Intn(3) + 1
		nTasks := r.Intn(6) + 1
		caps := map[string]int64{}
		capList := make([]int64, nNodes)
		for j := 0; j < nNodes; j++ {
			c := int64(r.Intn(8) + 1)
			caps[fmt.Sprintf("n%d", j)] = c
			capList[j] = c
		}
		reqs := make([]int64, nTasks)
		place := make([]*api.TaskInfo, nTasks)
		for i := 0; i < nTasks; i++ {
			g := int64(r.Intn(8) + 1)
			reqs[i] = g
			place[i] = gpuTask(i, "", g)
		}
		_, d := buildDomain(caps, nil)
		_, got := d.Feasible(place)
		want := bruteFeasible(reqs, capList, nil)
		if got != want {
			t.Fatalf("mismatch: solver=%v brute=%v reqs=%v caps=%v", got, want, reqs, capList)
		}
	}
}

// Same completeness check, now with a random Fit allow-map in the mix.
func TestFeasible_MatchesBruteForce_WithFit(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for iter := 0; iter < 3000; iter++ {
		nNodes := r.Intn(3) + 1
		nTasks := r.Intn(5) + 1
		caps := map[string]int64{}
		capList := make([]int64, nNodes)
		for j := 0; j < nNodes; j++ {
			c := int64(r.Intn(6) + 1)
			caps[fmt.Sprintf("n%d", j)] = c
			capList[j] = c
		}
		allow := map[string]map[string]bool{}
		allowFn := func(ti, nj int) bool {
			byNode := allow[fmt.Sprintf("t%d", ti)]
			if byNode == nil {
				return true
			}
			return byNode[fmt.Sprintf("n%d", nj)]
		}
		reqs := make([]int64, nTasks)
		place := make([]*api.TaskInfo, nTasks)
		for i := 0; i < nTasks; i++ {
			g := int64(r.Intn(6) + 1)
			reqs[i] = g
			place[i] = gpuTask(i, "", g)
			if r.Intn(2) == 0 { // half the tasks get a random allow-map
				byNode := map[string]bool{}
				for j := 0; j < nNodes; j++ {
					byNode[fmt.Sprintf("n%d", j)] = r.Intn(2) == 0
				}
				allow[fmt.Sprintf("t%d", i)] = byNode
			}
		}
		_, d := buildDomain(caps, allow)
		_, got := d.Feasible(place)
		want := bruteFeasible(reqs, capList, allowFn)
		if got != want {
			t.Fatalf("mismatch w/fit: solver=%v brute=%v reqs=%v caps=%v allow=%v", got, want, reqs, capList, allow)
		}
	}
}
