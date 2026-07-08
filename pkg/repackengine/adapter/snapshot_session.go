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
	v1 "k8s.io/api/core/v1"

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

// Predicate skips the scheduler's full PredicateFn stack for P0 drain feasibility.
// Resource fit is enforced by the drain ledger (Allocatable−Used); calling
// ssn.PredicateFn here false-negates feasible reshuffles because the bundled
// predicates plugin re-checks capacity via stale cache Idle/FutureIdle and runs
// PreFilter-dependent filters (DRA, inter-pod affinity) without the matching
// PreFilter state. Engine plugins may add constraint checks via AddPredicateFn
// in P1; until then nil means "constraints not modeled at this layer".
func (s *SessionSnapshot) Predicate(task *schedapi.TaskInfo, node *schedapi.NodeInfo) error {
	return nil
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
