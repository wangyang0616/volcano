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

// Package adapter is the ONLY scheduler-framework-coupled layer of the repack
// engine. It adapts a live volcano-scheduler Session into the framework's
// abstractions: a Snapshot (cluster view) and the gang-info source for scope
// resolution. Keeping every scheduler/framework
// import here lets api/ and framework/ stay pure and unit-testable.
package adapter

import (
	"context"
	"sort"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// SessionSnapshot adapts a live scheduler Session to framework.Snapshot. The
// standalone engine could instead build a Snapshot from informers; this is the
// in-scheduler-cache implementation. It applies the resolved scope.nodes filter
// so the core only ever sees in-scope nodes.
type SessionSnapshot struct {
	ssn      *schedframework.Session
	resource v1.ResourceName
	scope    *framework.ScopeMatcher // nil = all nodes in scope
}

var _ framework.Snapshot = (*SessionSnapshot)(nil)

// NewSessionSnapshot wraps a Session for the given target resource. scope gates
// drain targets (nil = all in scope); it does NOT filter the receiver set.
func NewSessionSnapshot(ssn *schedframework.Session, resource v1.ResourceName, scope *framework.ScopeMatcher) *SessionSnapshot {
	return &SessionSnapshot{ssn: ssn, resource: resource, scope: scope}
}

// Nodes returns ALL session nodes (the receiver universe). scope.nodes gates
// drain targets via NodeInScope, not the receiver set.
func (s *SessionSnapshot) Nodes() []*schedapi.NodeInfo {
	out := make([]*schedapi.NodeInfo, 0, len(s.ssn.Nodes))
	for _, n := range s.ssn.Nodes {
		if n != nil {
			out = append(out, n)
		}
	}
	return out
}

// NodeInScope reports whether a node may be a drain target (nil scope = all).
func (s *SessionSnapshot) NodeInScope(n *schedapi.NodeInfo) bool {
	return s.scope == nil || s.scope.NodeInScope(n)
}

// FeasibleReschedule simulates evicting `victims` and greedily rescheduling them
// onto `receivers`, with feasibility decided by the scheduler's FULL filter stack
// (SimulateFilterFn) — so a plan matches exactly what the scheduler will accept at
// Execute time. It runs entirely on CLONES (a node copy + a cycle-state copy per
// candidate), never mutating the shared session, which is the same isolation the
// preempt action relies on. `committed` are the relocations already decided earlier
// this pass; their pods count as present on their receiver nodes so capacity and
// topology stay consistent across steps. Resource fit is checked via FutureIdle
// (the scheduler's own model), everything else via SimulateFilterFn.
//
// `receivers` are tried in the ORDER GIVEN (first that fits wins): the caller is
// responsible for the receiver preference (e.g. the drain orders staying nodes
// first, then best-fit). This keeps the scheduler-feasibility concern here and the
// defrag placement policy at the call site.
//
// Returns the per-victim placements (from -> to) and whether every victim fit.
func (s *SessionSnapshot) FeasibleReschedule(committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	// Pods that already landed on each receiver this pass (prior committed moves).
	landed := map[string][]*schedapi.TaskInfo{}
	for _, m := range committed {
		if m != nil && m.Task != nil {
			landed[m.To] = append(landed[m.To], m.Task)
		}
	}

	ctx := context.TODO()
	placements := make([]*api.Move, 0, len(victims))
	for _, victim := range s.victimsLargestFirst(victims) {
		// Build the victim's PreFilter state once (running pods are not pre-inited
		// like pending pods are); every candidate then clones it.
		if err := s.ssn.PrePredicateFn(victim); err != nil {
			return nil, false
		}
		baseState := s.ssn.GetCycleState(victim.UID)

		target := s.firstFeasibleReceiver(ctx, victim, baseState, receivers, landed)
		if target == "" {
			return nil, false
		}
		landed[target] = append(landed[target], victim)
		placements = append(placements, &api.Move{Task: victim, From: victim.NodeName, To: target})
	}
	return placements, true
}

// firstFeasibleReceiver returns the first receiver (in the caller's preference
// order) that passes the full scheduler filters for victim, or "" if none fit.
func (s *SessionSnapshot) firstFeasibleReceiver(ctx context.Context, victim *schedapi.TaskInfo, baseState fwk.CycleState, receivers []*schedapi.NodeInfo, landed map[string][]*schedapi.TaskInfo) string {
	for _, node := range receivers {
		if s.victimFitsReceiver(ctx, victim, baseState, node, landed[node.Name]) {
			return node.Name
		}
	}
	return ""
}

// victimFitsReceiver checks, on CLONES only, whether victim can be scheduled onto
// node after the pods that already landed there this pass. Resource fit uses
// FutureIdle (the scheduler's own accounting); everything else — taints, node
// affinity, inter-pod affinity, topology spread, devices, volumes, DRA — is the
// full SimulateFilterFn stack.
func (s *SessionSnapshot) victimFitsReceiver(ctx context.Context, victim *schedapi.TaskInfo, baseState fwk.CycleState, node *schedapi.NodeInfo, alreadyLanded []*schedapi.TaskInfo) bool {
	nodeCopy := node.Clone()
	stateCopy := baseState.Clone()
	for _, pod := range alreadyLanded {
		if err := s.ssn.SimulateAddTaskFn(ctx, stateCopy, victim, pod, nodeCopy); err != nil {
			return false
		}
		if err := nodeCopy.AddTask(pod); err != nil {
			return false
		}
	}
	if !victim.InitResreq.LessEqual(nodeCopy.FutureIdle(), schedapi.Zero) {
		return false
	}
	return s.ssn.SimulateFilterFn(ctx, stateCopy, victim, nodeCopy) == nil
}

// victimsLargestFirst orders victims by descending target-resource request (first-
// fit-decreasing): place the biggest pods first to fail fast and pack tightly.
func (s *SessionSnapshot) victimsLargestFirst(victims []*schedapi.TaskInfo) []*schedapi.TaskInfo {
	ordered := append([]*schedapi.TaskInfo(nil), victims...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return api.Scalar(ordered[i].InitResreq, s.resource) > api.Scalar(ordered[j].InitResreq, s.resource)
	})
	return ordered
}


// PodGroupView reads MinAvailable/Running/Priority/Footprint off the JobInfo.
func (s *SessionSnapshot) PodGroupView(id schedapi.JobID) api.PodGroupView {
	ji, ok := s.ssn.Jobs[id]
	if !ok || ji == nil {
		return api.PodGroupView{}
	}
	var running int32
	if m, ok := ji.TaskStatusIndex[schedapi.Running]; ok {
		running = int32(len(m))
	}
	var footprint int64
	for _, t := range ji.Tasks {
		if t != nil && t.InitResreq != nil {
			footprint += scalar(t.InitResreq, s.resource)
		}
	}
	return api.PodGroupView{
		MinAvailable: ji.MinAvailable,
		Running:      running,
		Priority:     ji.Priority,
		Footprint:    footprint,
	}
}

// scalar returns the count of a single scalar resource on r (local copy so this
// package needs no exported helper from api for a one-line sum).
func scalar(r *schedapi.Resource, name v1.ResourceName) int64 {
	if r == nil || r.ScalarResources == nil {
		return 0
	}
	return int64(r.ScalarResources[name] + 0.5)
}
