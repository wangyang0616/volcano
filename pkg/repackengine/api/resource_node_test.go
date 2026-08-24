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

func TestClassifyTargetResourceNode(t *testing.T) {
	resourceName := v1.ResourceName("example.com/accelerator")
	resource := func(value int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: float64(value)}}
	}
	tests := []struct {
		name string
		node *schedapi.NodeInfo
		want TargetResourceNodeClass
	}{
		{name: "nil", want: TargetResourceNodeUnavailable},
		{name: "not providing", node: &schedapi.NodeInfo{Allocatable: resource(0)}, want: TargetResourceNodeUnavailable},
		{name: "empty", node: &schedapi.NodeInfo{Allocatable: resource(8), Used: resource(0)}, want: TargetResourceNodeEmpty},
		{name: "partial", node: &schedapi.NodeInfo{Allocatable: resource(8), Used: resource(4)}, want: TargetResourceNodePartial},
		{name: "full", node: &schedapi.NodeInfo{Allocatable: resource(8), Used: resource(8)}, want: TargetResourceNodeFull},
		{name: "overcommitted", node: &schedapi.NodeInfo{Allocatable: resource(8), Used: resource(9)}, want: TargetResourceNodeFull},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyTargetResourceNode(testCase.node, resourceName); got != testCase.want {
				t.Fatalf("classification=%q, want %q", got, testCase.want)
			}
		})
	}
}

func TestNodeFreeCapacityIgnoresStaleIdle(t *testing.T) {
	resourceName := v1.ResourceName("example.com/accelerator")
	node := &schedapi.NodeInfo{
		Allocatable: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: 8000}},
		Used:        &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: 4000}},
		Idle:        schedapi.EmptyResource(),
		Releasing:   schedapi.EmptyResource(),
		Pipelined:   schedapi.EmptyResource(),
	}
	if got := Scalar(NodeFreeCapacity(node), resourceName); got != 4000 {
		t.Fatalf("free accelerator = %d, want 4000", got)
	}
	if got := Scalar(node.FutureIdle(), resourceName); got != 0 {
		t.Fatalf("FutureIdle accelerator = %d, want 0 to demonstrate stale Idle", got)
	}
}
