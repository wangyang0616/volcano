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

package framework

import (
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// CommitPlan evicts freed-node sources first and is open-loop: a failed eviction
// is recorded, not fatal, and the rest proceed.
func TestCommitPlan_EvictOrderAndOpenLoop(t *testing.T) {
	a := move(task("a", "ga", 1), "n0", "n2") // n0 is freed → evicted first
	b := move(task("b", "gb", 1), "n1", "n2")
	plan := &api.RepackPlan{Moves: []*api.Move{b, a}, FreedNodes: []string{"n0"}}

	var order []string
	hooks := CommitHooks{Evict: func(m *api.Move) error {
		order = append(order, m.Task.Name)
		if m.Task.Name == "b" {
			return fmt.Errorf("evict b failed")
		}
		return nil
	}}

	res, err := CommitPlan(plan, hooks)
	if err != nil {
		t.Fatalf("open-loop commit must not error: %v", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("evict order=%v, want [a b] (freed-node source first)", order)
	}
	if len(res.Evicted) != 1 || res.Evicted[0].Task != "a" {
		t.Errorf("evicted=%+v, want [a]", res.Evicted)
	}
	if len(res.Failed) != 1 || res.Failed[0].Task != "b" {
		t.Errorf("failed=%+v, want [b]", res.Failed)
	}
}

// A missing Evict hook in Execute is a hard error.
func TestCommitPlan_NilEvictErrors(t *testing.T) {
	plan := &api.RepackPlan{Moves: []*api.Move{move(task("a", "ga", 1), "n0", "n1")}}
	if _, err := CommitPlan(plan, CommitHooks{}); err == nil {
		t.Error("nil Evict must error")
	}
}

// NominationIntents carries the identity keys the reconciler matches by.
func TestNominationIntents(t *testing.T) {
	a := &api.Move{
		Task: &schedapi.TaskInfo{Name: "job-w-0", Namespace: "ns", Job: "ns/g", TaskRole: "worker"},
		From: "n0", To: "n2",
	}
	plan := &api.RepackPlan{Moves: []*api.Move{a}, FreedNodes: []string{"n0"}}

	in := NominationIntents(plan)
	if len(in) != 1 {
		t.Fatalf("intents=%d, want 1", len(in))
	}
	got := in[0]
	if got.Namespace != "ns" || got.PodName != "job-w-0" || got.Gang != "ns/g" || got.Role != "worker" || got.Node != "n2" {
		t.Errorf("intent=%+v", got)
	}
}

// resolveIdentityLabels reads only the pod's own well-known labels, in priority
// order (Tier-1 declarative > native index labels), else nil (fungible).
func TestResolveIdentityLabels(t *testing.T) {
	pod := func(l map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: l}}
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want map[string]string
	}{
		{"nil pod", nil, nil},
		{"no labels -> fungible", pod(nil), nil},
		{"tier1 wins over native", pod(map[string]string{
			labelPodIdentity:         "worker-3",
			labelStatefulSetPodIndex: "3",
		}), map[string]string{labelPodIdentity: "worker-3"}},
		{"statefulset pod-index", pod(map[string]string{labelStatefulSetPodIndex: "2"}),
			map[string]string{labelStatefulSetPodIndex: "2"}},
		{"indexed job completion-index", pod(map[string]string{labelJobCompletionIndex: "5"}),
			map[string]string{labelJobCompletionIndex: "5"}},
		{"empty value ignored -> fungible", pod(map[string]string{labelPodIdentity: ""}), nil},
	}
	for _, c := range cases {
		if got := resolveIdentityLabels(c.pod); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
