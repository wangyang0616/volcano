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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Landing-identity contract well-known label keys (§5.2.2). resolveIdentityLabels
// reads only these standard keys off the pod itself — no per-workload label scheme
// is hardcoded.
const (
	// labelPodIdentity is the declarative Tier-1 identity (workload-provided).
	labelPodIdentity = "repack.volcano.sh/pod-identity"
	// labelStatefulSetPodIndex / labelJobCompletionIndex are the standard K8s
	// per-pod index labels used as native-adapter identities.
	labelStatefulSetPodIndex = "apps.kubernetes.io/pod-index"
	labelJobCompletionIndex  = "batch.kubernetes.io/job-completion-index"
)

// Nomination is a landing hint written on a concrete pending pod
// (pod.status.NominatedNodeName). The steering primitive, written by the
// nomination reconciler on each *replacement* pod, not on the dying victim.
type Nomination struct {
	PodRef string // "namespace/name"
	Node   string
}

// NominationIntent is the durable steering record for ONE relocated pod: after
// the victim is evicted, its replacement should be nominated to Node. Carries the
// keys the reconciler matches the replacement by, per the landing-identity
// contract (§5.2.2): exact PodName (same-name rebuild), else IdentityLabels
// (label-superset match), else fungible (any pod in the gang). Role is retained
// for audit/debug only.
type NominationIntent struct {
	Namespace      string
	PodName        string
	Gang           schedapi.JobID
	Role           string
	IdentityLabels map[string]string
	Node           string
}

// NominationIntents derives one intent per relocated pod (To != From) in
// deterministic order (namespace, gang, podName, node).
func NominationIntents(plan *api.RepackPlan) []NominationIntent {
	if plan == nil {
		return nil
	}
	out := make([]NominationIntent, 0, len(plan.Moves))
	for _, m := range orderedMoves(plan) {
		t := m.Task
		out = append(out, NominationIntent{
			Namespace:      t.Namespace,
			PodName:        t.Name,
			Gang:           t.Job,
			Role:           t.TaskRole,
			IdentityLabels: resolveIdentityLabels(t.Pod),
			Node:           m.To,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		case a.Gang != b.Gang:
			return a.Gang < b.Gang
		case a.PodName != b.PodName:
			return a.PodName < b.PodName
		default:
			return a.Node < b.Node
		}
	})
	return out
}

// resolveIdentityLabels returns the labels that identify a pod's replacement
// across reconstruction, per the landing-identity contract (§5.2.2), reading only
// the pod's own well-known labels in priority order:
//  1. Tier 1: the declarative repack.volcano.sh/pod-identity label (workloads opt in);
//  2. native adapters: the standard StatefulSet pod-index / Indexed Job
//     completion-index labels (already present on those pods);
//  3. fungible: nil (any pending pod in the PodGroup).
//
// The result is self-describing (the label key+value are visible in status) and
// the reconciler matches whatever key is recorded — no per-workload scheme is
// hardcoded. Empty/nil = fungible.
func resolveIdentityLabels(pod *corev1.Pod) map[string]string {
	if pod == nil {
		return nil
	}
	for _, key := range []string{labelPodIdentity, labelStatefulSetPodIndex, labelJobCompletionIndex} {
		if v, ok := pod.Labels[key]; ok && v != "" {
			return map[string]string{key: v}
		}
	}
	return nil
}

// MoveOutcome records one move's commit result (engine-internal).
type MoveOutcome struct {
	Task string
	From string
	To   string
	Err  string // non-empty if the eviction failed (open-loop: not fatal)
}

// CommitResult is what the commit attempted — raw material for status.result.
type CommitResult struct {
	Evicted   []MoveOutcome
	Failed    []MoveOutcome
	Nominated []Nomination
}

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
		oc := MoveOutcome{Task: taskName(m), From: m.From, To: m.To}
		if err := h.Evict(m); err != nil {
			oc.Err = err.Error()
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

// pendingNominations: landing hints for pods Pending at commit time (relief, added later).
// Empty for consolidation (replacement pods don't exist yet → steered async by
// the nomination reconciler over NominationIntents).
func pendingNominations(_ *api.RepackPlan) []Nomination { return nil }

func taskName(m *api.Move) string {
	if m == nil || m.Task == nil {
		return ""
	}
	return m.Task.Name
}
