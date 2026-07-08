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
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// OptimalNodes is the fragmentation packing bound — the hot inner call of every
// MeasureResource. Bench a large mixed request set.
func BenchmarkOptimalNodes(b *testing.B) {
	reqs := make([]int64, 0, 2000)
	for i := 0; i < 2000; i++ {
		reqs = append(reqs, int64(1<<(i%4))) // 1,2,4,8 cards
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		OptimalNodes(reqs, 8)
	}
}

// Feasible is the INV-RESCHED backtracking solver — the most expensive per-plan
// step. Bench placing many victims across a large receiver domain.
func BenchmarkFeasible(b *testing.B) {
	caps := make(map[string]int64, 200)
	for i := 0; i < 200; i++ {
		caps[fmt.Sprintf("n%d", i)] = 8
	}
	place := make([]*schedapi.TaskInfo, 0, 64)
	for i := 0; i < 64; i++ {
		place = append(place, gpuTask(i, "src", 2))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, dom := buildDomain(caps, nil) // fresh ledger each iteration
		dom.Feasible(place)
	}
}
