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
	"fmt"

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

func gpuRes(n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

func gpuTask(idx int, from string, g int64) *schedapi.TaskInfo {
	t := &schedapi.TaskInfo{Name: fmt.Sprintf("t%d", idx), InitResreq: gpuRes(g)}
	t.NodeName = from
	return t
}

func capNode(name string, capGPU int64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	m := map[schedapi.TaskID]*schedapi.TaskInfo{}
	var used int64
	for i, t := range tasks {
		t.NodeName = name
		m[schedapi.TaskID(fmt.Sprintf("%s-%d", name, i))] = t
		used += int64(t.InitResreq.ScalarResources[gpu] + 0.5)
	}
	return &schedapi.NodeInfo{
		Name:        name,
		Tasks:       m,
		Allocatable: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(capGPU)}},
		Used:        &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(used)}},
	}
}
