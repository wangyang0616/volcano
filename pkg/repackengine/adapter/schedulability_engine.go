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

// NOTE: this file is a RESERVED Statement-based feasibility oracle. It is NOT on
// the P0 drain path — the drain (core/drain) does its own incremental, gang-aware
// feasibility via framework.Session.Predicate + api.Domain.Feasible over a running
// ledger, so ValidatePlan/EngineFit are currently unused. It is kept because it
// additionally runs ssn.PrePredicateFn (a per-task pre-check the P0 drain path
// skips — a known P0 feasibility gap, e.g. DRA/claim prerequisites) and drives a
// real Statement.Evict; P1's relief/simulate actions can adopt it. If P1 does not,
// delete this file. Untested while unused.

import (
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// EngineFit adapts a live Session's predicates into an api.Fit for the solver.
// Per-node predicate covers affinity/taints/topology/device; resource fit is
// handled separately by the solver's ledger.
func EngineFit(ssn *schedframework.Session) api.Fit {
	return func(task *schedapi.TaskInfo, node *schedapi.NodeInfo) bool {
		return ssn.PredicateFn(task, node) == nil
	}
}

// ValidatePlan dry-runs a rearrangement against the live session and reports
// whether INV-RESCHED holds: after evicting victims, every task in place can be
// (re)scheduled onto a surviving node. Non-destructive — the Statement is always
// discarded. Returns where each task would land; the caller turns those into soft
// nominations when it decides to act.
func ValidatePlan(
	ssn *schedframework.Session,
	victims []*schedapi.TaskInfo,
	place []*schedapi.TaskInfo,
	nodes []*schedapi.NodeInfo,
) (moves []*api.Move, ok bool) {
	stmt := schedframework.NewStatement(ssn)
	defer stmt.Discard()

	for _, v := range victims {
		stmt.Evict(v, "repack-simulate")
	}
	for _, t := range place {
		if err := ssn.PrePredicateFn(t); err != nil {
			return nil, false
		}
	}
	d := api.NewDomain(nodes, func(n *schedapi.NodeInfo) *schedapi.Resource { return n.FutureIdle() }, EngineFit(ssn))
	return d.Feasible(place)
}
