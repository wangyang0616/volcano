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

// BenchmarkDrainScale isolates the cost of a bounded number of greedy steps in
// a large receiver universe. Every node starts 1/4 full and owns a distinct
// PodGroup, so maxPodGroups=N limits the pass to exactly N committed units.
func BenchmarkDrainScale(b *testing.B) {
	cases := []struct {
		name       string
		totalNodes int
		scopeNodes int
		maxGroups  int
		failFirst  int
	}{
		{name: "total4000_scope250_steps1", totalNodes: 4000, scopeNodes: 250, maxGroups: 1},
		{name: "total4000_scope250_steps4", totalNodes: 4000, scopeNodes: 250, maxGroups: 4},
		{name: "total4000_scope1000_steps1", totalNodes: 4000, scopeNodes: 1000, maxGroups: 1},
		{name: "total4000_scope4000_steps1", totalNodes: 4000, scopeNodes: 4000, maxGroups: 1},
		{name: "total4000_scope4000_fail32_steps1", totalNodes: 4000, scopeNodes: 4000, maxGroups: 1, failFirst: 32},
		{name: "total4000_scope4000_steps4", totalNodes: 4000, scopeNodes: 4000, maxGroups: 4},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			nodes := make([]*schedapi.NodeInfo, 0, tc.totalNodes)
			views := make(map[schedapi.JobID]api.PodGroupView, tc.totalNodes)
			notInScope := make(map[string]bool, tc.totalNodes-tc.scopeNodes)
			for i := 0; i < tc.totalNodes; i++ {
				name := fmt.Sprintf("n%04d", i)
				podGroup := schedapi.JobID(fmt.Sprintf("g%04d", i))
				nodes = append(nodes, capNode(name, 8, gpuTask(fmt.Sprintf("t%04d", i), string(podGroup), 2)))
				views[podGroup] = api.PodGroupView{Running: 1, MinAvailable: 1, Footprint: 2}
				if i >= tc.scopeNodes {
					notInScope[name] = true
				}
			}

			var totalCalls, totalFreed int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				infeasibleSources := make(map[string]bool, tc.failFirst)
				for source := 0; source < tc.failFirst; source++ {
					infeasibleSources[fmt.Sprintf("n%04d", source)] = true
				}
				snap := &fakeSnap{nodes: nodes, views: views, notInScope: notInScope, infeasibleSources: infeasibleSources}
				plan, _ := (&drainCore{}).Plan(drainSessionWithPlugins(
					snap, allMovable, 1, tc.maxGroups, 0, []string{"base", "gang"},
				))
				totalCalls += snap.feasibilityCalls
				if plan != nil {
					totalFreed += len(plan.FreedNodes)
				}
			}
			b.ReportMetric(float64(totalCalls)/float64(b.N), "feasibility/op")
			b.ReportMetric(float64(totalFreed)/float64(b.N), "freed/op")
		})
	}
}
