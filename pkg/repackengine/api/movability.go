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

import "volcano.sh/volcano/pkg/scheduler/api"

// Movable reports whether a running task may be moved by repack (design §4.5
// scope.podGroups include/exclude, gang/PDB rules). Tasks of frozen PodGroups
// return false and are never evicted. A nil Movable means everything is movable.
// The engine Session aggregates the MovableFns registered by plugins (AND: any
// plugin may veto a move) into a single Movable for the core to consume.
type Movable func(task *api.TaskInfo) bool

// NodeFreeable reports whether a node can be fully vacated: it hosts at least one
// task and every task on it is movable. A node carrying any frozen task can never
// be emptied, so it is dropped from the free-set before the search.
func NodeFreeable(node *api.NodeInfo, movable Movable) bool {
	if node == nil || len(node.Tasks) == 0 {
		return false
	}
	for _, t := range node.Tasks {
		if t == nil {
			continue
		}
		if movable != nil && !movable(t) {
			return false
		}
	}
	return true
}

// VictimsOf returns the movable tasks that vacating node would displace.
func VictimsOf(node *api.NodeInfo, movable Movable) []*api.TaskInfo {
	if node == nil {
		return nil
	}
	out := make([]*api.TaskInfo, 0, len(node.Tasks))
	for _, t := range node.Tasks {
		if t == nil {
			continue
		}
		if movable == nil || movable(t) {
			out = append(out, t)
		}
	}
	return out
}
