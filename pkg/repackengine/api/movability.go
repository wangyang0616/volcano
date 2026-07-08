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
	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// Movable reports whether a running task may be moved by repack (design §4.5
// scope.podGroups include/exclude, gang/PDB rules). Tasks of frozen PodGroups
// return false and are never evicted. A nil Movable means everything is movable.
// The engine Session aggregates the MovableFns registered by plugins (AND: any
// plugin may veto a move) into a single Movable for the core to consume.
type Movable func(task *api.TaskInfo) bool

// NodeFreeable reports whether a node's target-resource (res) capacity can be
// vacated: it hosts at least one task USING res and every such task is movable.
//
// Only tasks requesting res count. The scheduler cache tracks every pod on a node
// (system DaemonSets — kube-proxy, CNI — included), and those have no PodGroup so
// they are never movable; requiring all pods movable would make every real
// accelerator node unfreeable. "Freeing" a node for defrag means vacating its
// accelerator, not removing pinned pods — the node keeps running its DaemonSets,
// its accelerator just goes idle. A node with a frozen accelerator task can never
// be freed (its card stays pinned).
func NodeFreeable(node *api.NodeInfo, movable Movable, res v1.ResourceName) bool {
	if node == nil {
		return false
	}
	hasAccelerator := false
	for _, t := range node.Tasks {
		if t == nil || Scalar(t.InitResreq, res) <= 0 {
			continue // non-accelerator pod (DaemonSet/CPU-only): irrelevant to freeing res
		}
		hasAccelerator = true
		if movable != nil && !movable(t) {
			return false
		}
	}
	return hasAccelerator
}

// VictimsOf returns the movable target-resource (res) tasks that vacating node
// would displace. Non-accelerator pods (DaemonSets/CPU-only) stay on the node and
// are not returned — only the accelerator pods need relocating.
func VictimsOf(node *api.NodeInfo, movable Movable, res v1.ResourceName) []*api.TaskInfo {
	if node == nil {
		return nil
	}
	out := make([]*api.TaskInfo, 0, len(node.Tasks))
	for _, t := range node.Tasks {
		if t == nil || Scalar(t.InitResreq, res) <= 0 {
			continue
		}
		if movable == nil || movable(t) {
			out = append(out, t)
		}
	}
	return out
}
