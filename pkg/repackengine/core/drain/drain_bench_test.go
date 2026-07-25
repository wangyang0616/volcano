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

package drain

import (
	"fmt"
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// BenchmarkDrain measures the end-to-end incremental gang-aware drain over a
// fragmented cluster: N nodes each 1/4 full with a small gang, so many can be
// consolidated (exercises the dynamic re-pick + feasibility solver at scale).
func BenchmarkDrain(b *testing.B) {
	for _, n := range []int{25, 100, 250} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			nodes := make([]*schedapi.NodeInfo, 0, n)
			views := make(map[schedapi.JobID]api.PodGroupView, n)
			for i := 0; i < n; i++ {
				podGroup := schedapi.JobID(fmt.Sprintf("g%d", i))
				nodes = append(nodes, capNode(
					fmt.Sprintf("n%d", i), 8,
					gpuTask(fmt.Sprintf("t%d", i), string(podGroup), 2),
				))
				views[podGroup] = api.PodGroupView{Running: 1, MinAvailable: 1, Footprint: 2}
			}
			snap := &fakeSnap{nodes: nodes, views: views}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				(&drainCore{}).Plan(drainSessionWithPlugins(
					snap, allMovable, 1, 0, 0, []string{"base", "gang"},
				))
			}
		})
	}
}
