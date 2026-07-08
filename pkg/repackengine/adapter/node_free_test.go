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

package adapter

import (
	"testing"

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// When Idle is stale (zero) but Used/Allocatable reflect bound pods, repack
// planning must still see slack via Allocatable − Used.
func TestNodeFreeCapacity_IgnoresStaleIdle(t *testing.T) {
	const npu = v1.ResourceName("volcano.sh/e2e-npu")
	node := &schedapi.NodeInfo{
		Name:        "n0",
		Allocatable: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{npu: 8000}},
		Used:        &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{npu: 4000}},
		Idle:        schedapi.EmptyResource(), // stale: NPU never credited
		Releasing:   schedapi.EmptyResource(),
		Pipelined:   schedapi.EmptyResource(),
	}
	free := NodeFreeCapacity(node)
	if got := nodeScalar(free, npu); got != 4000 {
		t.Fatalf("free NPU = %v, want 4000 (8000 alloc − 4000 used)", got)
	}
	if got := nodeScalar(node.FutureIdle(), npu); got != 0 {
		t.Fatalf("FutureIdle NPU = %v, want 0 to show Idle is stale", got)
	}
}

func nodeScalar(r *schedapi.Resource, name v1.ResourceName) float64 {
	if r == nil || r.ScalarResources == nil {
		return 0
	}
	return r.ScalarResources[name]
}
