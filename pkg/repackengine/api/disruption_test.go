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
	"testing"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// gpuJobTask builds a task of gang `gang` requesting g GPUs (gpuRes/gpu come from
// the package's other test files).
func gpuJobTask(name, gang string, g int64) *api.TaskInfo {
	return &api.TaskInfo{Name: name, Job: api.JobID(gang), InitResreq: gpuRes(g)}
}

// MoveAggregate counts real relocations per gang; To==From is ignored.
func TestAggregate(t *testing.T) {
	ctx := &PlanContext{TargetResource: gpu}
	cp := &CandidatePlan{Moves: []*Move{
		{Task: gpuJobTask("a", "g1", 2), From: "n0", To: "n1"},
		{Task: gpuJobTask("b", "g1", 3), From: "n0", To: "n1"},
		{Task: gpuJobTask("c", "g2", 4), From: "n0", To: "n1"},
		{Task: gpuJobTask("d", "g2", 1), From: "n0", To: "n0"}, // no-op
	}}
	a := cp.MoveAggregate(ctx)
	if a.AffectedPodGroups != 2 {
		t.Errorf("affectedPGs=%d, want 2", a.AffectedPodGroups)
	}
	if a.MovedPods != 3 {
		t.Errorf("movedPods=%d, want 3", a.MovedPods)
	}
	if a.MovedResource != 9 {
		t.Errorf("movedResource=%d, want 9", a.MovedResource)
	}
	if g1 := a.ByPodGroup["g1"]; g1 == nil || g1.MovedPods != 2 || g1.MovedResource != 5 {
		t.Errorf("g1=%+v, want {2 pods, 5 gpu}", g1)
	}
	if g2 := a.ByPodGroup["g2"]; g2 == nil || g2.MovedPods != 1 || g2.MovedResource != 4 {
		t.Errorf("g2=%+v, want {1 pod, 4 gpu}", g2)
	}
}

func TestAggregateIncludesCommittedAndIncrementalMoves(t *testing.T) {
	ctx := &PlanContext{TargetResource: gpu}
	plan := &CandidatePlan{
		CommittedMoves: []*Move{{Task: gpuJobTask("committed", "g1", 2), From: "n0", To: "n1"}},
		Moves:          []*Move{{Task: gpuJobTask("candidate", "g2", 3), From: "n2", To: "n3"}},
	}
	aggregate := plan.MoveAggregate(ctx)
	if aggregate.MovedPods != 2 || aggregate.MovedResource != 5 || aggregate.AffectedPodGroups != 2 {
		t.Fatalf("aggregate=%+v, want two moves across two PodGroups", aggregate)
	}
}

// CalculateDisruptionCost summarizes a move set's default dimensions.
func TestCostOf(t *testing.T) {
	c := CalculateDisruptionCost([]*Move{
		{Task: gpuJobTask("a", "g1", 2), From: "n0", To: "n1"},
		{Task: gpuJobTask("c", "g2", 4), From: "n0", To: "n1"},
	}, gpu)
	if c.AffectedPodGroups != 2 || c.MovedResource != 6 || c.MovedPods != 2 {
		t.Errorf("cost=%+v, want {2, 6, 2}", c)
	}
}
