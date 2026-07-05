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

// Package framework is the repack engine's plugin/action framework — the
// analogue of pkg/scheduler/framework. It defines the engine Session (a plugin-
// populated, action-consumed view of one repack pass), the three extension-point
// contracts (Plugin = capability, Action = pipeline stage, Core = single-select
// search strategy) and their registries, plus the commit (evict/nominate) and
// report plumbing. It depends only on pkg/repackengine/api (pure model) and
// pkg/scheduler/api (NodeInfo/TaskInfo) — never on the scheduler framework; the
// live-Session coupling lives in pkg/repackengine/session.
package framework

import (
	"sort"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Snapshot is the read-only cluster view a repack pass plans against. Abstracting
// it (instead of the scheduler cache/Session) lets the standalone engine build a
// view from informers; a live scheduler Session is one implementation (see
// pkg/repackengine/session), tests use a fake. Implementations must be safe for
// concurrent reads during one pass.
type Snapshot interface {
	// Nodes returns ALL candidate nodes — the receiver universe. scope.nodes
	// gates drain TARGETS (via NodeInScope), not the receiver set: a node the
	// user excludes from draining can still receive relocated pods.
	Nodes() []*schedapi.NodeInfo
	// NodeInScope reports whether a node may be a drain TARGET (scope.nodes
	// include − exclude). Out-of-scope nodes stay as (preferred) receivers.
	// True when no node scope is set.
	NodeInScope(node *schedapi.NodeInfo) bool
	// PodGroupView returns gang/priority facts for a PodGroup (zero if absent).
	PodGroupView(schedapi.JobID) api.PodGroupView
	// Predicate reports node fit for affinity/taint/topology/device constraints
	// (nil = fits). Resource fit is handled by the core's own ledger.
	Predicate(task *schedapi.TaskInfo, node *schedapi.NodeInfo) error
}

// MovableInScope builds an api.Movable from scope predicates: a task is movable
// iff its PodGroup is in scope (scope.podGroups include − exclude) and — P1 — not
// blocked by PDB. inScope nil = all PodGroups; pdbBlocks nil = ignore PDB.
func MovableInScope(inScope func(schedapi.JobID) bool, pdbBlocks func(*schedapi.TaskInfo) bool) api.Movable {
	return func(t *schedapi.TaskInfo) bool {
		if t == nil {
			return false
		}
		if inScope != nil && !inScope(t.Job) {
			return false
		}
		if pdbBlocks != nil && pdbBlocks(t) { // P1 seam
			return false
		}
		return true
	}
}

// NodesInScope filters nodes by nodeInScope (scope.nodes include − exclude; nil =
// all) and returns them in stable name order for deterministic planning.
func NodesInScope(nodes []*schedapi.NodeInfo, nodeInScope func(*schedapi.NodeInfo) bool) []*schedapi.NodeInfo {
	out := make([]*schedapi.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if nodeInScope != nil && !nodeInScope(n) {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
