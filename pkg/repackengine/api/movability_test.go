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

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

var movabilityGPU = v1.ResourceName("nvidia.com/gpu")

func TestEvaluateNodeFreeability(t *testing.T) {
	resource := func(gpu float64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{movabilityGPU: gpu}}
	}
	task := func(name string, gpu float64) *schedapi.TaskInfo {
		return &schedapi.TaskInfo{Name: name, Namespace: "workload", Job: "workload/group-a", InitResreq: resource(gpu)}
	}
	node := func(tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
		items := make(map[schedapi.TaskID]*schedapi.TaskInfo, len(tasks))
		for _, item := range tasks {
			items[schedapi.TaskID(item.Name)] = item
		}
		return &schedapi.NodeInfo{Name: "n0", Tasks: items}
	}
	movable := func(task *schedapi.TaskInfo) bool { return task.Name != "pinned" }
	pinned := task("pinned", 1)

	tests := []struct {
		name       string
		node       *schedapi.NodeInfo
		state      NodeFreeabilityState
		wantFree   bool
		wantReason NodeFreeabilityReason
		wantPinned int
	}{
		{name: "missing node", wantReason: NodeNotFoundReason},
		{name: "already drained", node: node(task("movable", 1)), state: NodeFreeabilityState{Drained: true}, wantReason: AlreadyDrainedReason},
		{name: "receiver", node: node(task("movable", 1)), state: NodeFreeabilityState{Filled: true}, wantReason: SelectedAsReceiverReason},
		{name: "no target resource pod", node: node(task("cpu-only", 0)), wantReason: NoTargetResourcePodReason},
		{name: "immovable target resource pod", node: node(pinned), wantReason: HasImmovableTargetResourcePodReason, wantPinned: 1},
		{name: "freeable", node: node(task("movable", 1)), wantFree: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := EvaluateNodeFreeability(testCase.node, testCase.state, movable, movabilityGPU)
			if got.Freeable != testCase.wantFree || got.Reason != testCase.wantReason {
				t.Fatalf("result=%+v, want freeable=%t reason=%q", got, testCase.wantFree, testCase.wantReason)
			}
			if len(got.ImmovableTasks) != testCase.wantPinned {
				t.Fatalf("immovable tasks=%d, want %d", len(got.ImmovableTasks), testCase.wantPinned)
			}
		})
	}
}
