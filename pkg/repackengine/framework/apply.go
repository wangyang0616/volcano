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

// Execute-mode commit (evict + nominate) — design §4.7.1.
//
// repack is open-loop advisory steering, not a reservation: feasibility is
// guaranteed BEFORE eviction (INV-RESCHED), the moved pods are then *steered* to
// their planned destinations via nomination, and the freed space is left OPEN for
// the scheduler's pending queue — the whole point. repack never taints/holds/
// cordons. CommitPlan is the deterministic commit of one plan; the real side
// effects (Eviction API, NominatedNodeName patch) are injected as CommitHooks so
// this stays unit-testable and free of client/framework deps. DryRun never calls it.
package framework

import (
	"errors"
	"fmt"
	"sort"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Nomination is a placement hint written on a concrete pending pod
// (pod.status.NominatedNodeName). The steering primitive, written by the
// nomination reconciler on each *replacement* pod, not on the dying victim.
type Nomination struct {
	PodRef string // "namespace/name"
	Node   string
}

// MoveOutcome records one move's commit result (engine-internal).
type MoveOutcome struct {
	Namespace         string
	PodGroupID        string
	PodName           string
	SourceNode        string
	TargetNode        string
	Err               string // non-empty if the eviction failed (open-loop: not fatal)
	VictimPodNotFound bool   // the victim disappeared while the commit was in progress
}

// CommitResult is what the commit attempted — raw material for status.result.
type CommitResult struct {
	Evicted   []MoveOutcome
	Failed    []MoveOutcome
	Nominated []Nomination
}

// ErrVictimNotFound preserves the semantic reason of a Kubernetes NotFound
// response across the framework's generic eviction hook. The engine later
// distinguishes a workload-level cascade from an unrelated failed eviction.
var ErrVictimNotFound = errors.New("repack victim Pod not found")

// CommitHooks are the injected side effects. Evict is required; Nominate is for
// relief pending targets (added later). Funcs so production supplies Eviction-API /
// status-patch impls while tests use fakes.
type CommitHooks struct {
	Evict    func(m *api.Move) error
	Nominate func(n Nomination) error
}

// CommitPlan applies a plan's moves node-freeing-first and returns what was done.
// Open-loop: a failed eviction is recorded and the commit continues. It does NOT
// hold/taint freed nodes — that space is intentionally left open for the queue.
func CommitPlan(plan *api.RepackPlan, h CommitHooks) (CommitResult, error) {
	var res CommitResult
	if plan == nil {
		return res, nil
	}
	if h.Evict == nil {
		return res, fmt.Errorf("repack: CommitHooks.Evict is required for Execute")
	}
	if h.Nominate != nil {
		for _, n := range pendingNominations(plan) {
			if err := h.Nominate(n); err != nil {
				return res, fmt.Errorf("repack: nominate %s->%s: %w", n.PodRef, n.Node, err)
			}
			res.Nominated = append(res.Nominated, n)
		}
	}
	for _, m := range orderedMoves(plan) {
		oc := MoveOutcome{
			Namespace:  m.Task.Namespace,
			PodGroupID: string(m.Task.Job),
			PodName:    taskName(m),
			SourceNode: m.From,
			TargetNode: m.To,
		}
		if err := h.Evict(m); err != nil {
			oc.Err = err.Error()
			oc.VictimPodNotFound = errors.Is(err, ErrVictimNotFound)
			res.Failed = append(res.Failed, oc)
			continue
		}
		res.Evicted = append(res.Evicted, oc)
	}
	return res, nil
}

// orderedMoves returns real relocations with freed-node sources first, then
// stable by task name (deterministic commit order; DryRun preview == Execute).
func orderedMoves(plan *api.RepackPlan) []*api.Move {
	freed := make(map[string]bool, len(plan.FreedNodes))
	for _, n := range plan.FreedNodes {
		freed[n] = true
	}
	ms := make([]*api.Move, 0, len(plan.Moves))
	for _, m := range plan.Moves {
		if m != nil && m.Task != nil && m.To != m.From {
			ms = append(ms, m)
		}
	}
	sort.SliceStable(ms, func(i, j int) bool {
		fi, fj := freed[ms[i].From], freed[ms[j].From]
		if fi != fj {
			return fi
		}
		return taskName(ms[i]) < taskName(ms[j])
	})
	return ms
}

// pendingNominations: placement hints for pods Pending at commit time (relief, added later).
// Empty for consolidation (replacement pods don't exist yet → steered async by
// the nomination reconciler from RepackRun status).
func pendingNominations(_ *api.RepackPlan) []Nomination { return nil }

func taskName(m *api.Move) string {
	if m == nil || m.Task == nil {
		return ""
	}
	return m.Task.Name
}
