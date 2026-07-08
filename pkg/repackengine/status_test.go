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

package repackengine

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const gpuResource = v1.ResourceName("nvidia.com/gpu")

func mkMove(name, job string, cards float64, from, to string) *engineapi.Move {
	return &engineapi.Move{
		Task: &schedapi.TaskInfo{
			Name: name, Job: schedapi.JobID(job),
			Resreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: cards}},
		},
		From: from, To: to,
	}
}

// pct rounds a 0-1 fraction to a percentage point, clamped to [0,100].
func TestPct(t *testing.T) {
	cases := []struct {
		in   float64
		want int32
	}{
		{0, 0}, {0.5, 50}, {1, 100},
		{0.004, 0}, {0.005, 1}, // round-half-up at the 0.5pp boundary
		{-0.3, 0},  // clamp low
		{1.7, 100}, // clamp high
	}
	for _, c := range cases {
		if got := pct(c.in); got != c.want {
			t.Errorf("pct(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitJobID(t *testing.T) {
	if ns, n := splitJobID("team-a/gang-1"); ns != "team-a" || n != "gang-1" {
		t.Errorf("split=%q/%q, want team-a/gang-1", ns, n)
	}
	if ns, n := splitJobID("bare"); ns != "" || n != "bare" {
		t.Errorf("split no-slash=%q/%q, want ''/bare", ns, n)
	}
	if ns, n := splitJobID(""); ns != "" || n != "" {
		t.Errorf("split empty=%q/%q, want ''/''", ns, n)
	}
}

func TestFreedNodesOf(t *testing.T) {
	if freedNodesOf(nil) != nil {
		t.Error("nil plan -> nil")
	}
	got := freedNodesOf(&engineapi.RepackPlan{FreedNodes: []string{"n2", "n0", "n1"}})
	if len(got) != 3 || got[0] != "n0" || got[1] != "n1" || got[2] != "n2" {
		t.Errorf("freedNodes=%v, want sorted [n0 n1 n2]", got)
	}
}

func TestMovesOf(t *testing.T) {
	// nil plan / no moves.
	if movesOf(nil, gpuResource) != nil {
		t.Error("nil plan -> nil")
	}

	// A gang whose pods spread across nodes; a no-op move (To==From) is dropped;
	// two gangs come out namespace/name-sorted.
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		mkMove("w1", "ns/gb", 2, "n0", "n2"),
		mkMove("w0", "ns/gb", 2, "n1", "n2"), // same gang, different source node
		mkMove("a0", "ns/ga", 4, "n0", "n3"),
		mkMove("noop", "ns/ga", 1, "n5", "n5"), // To==From: filtered
	}}
	moves := movesOf(plan, gpuResource)
	if len(moves) != 2 {
		t.Fatalf("got %d podgroup moves, want 2", len(moves))
	}
	// sorted by (namespace, podGroupName): ga before gb.
	if moves[0].PodGroupName != "ga" || moves[1].PodGroupName != "gb" {
		t.Fatalf("order=%s,%s want ga,gb", moves[0].PodGroupName, moves[1].PodGroupName)
	}
	ga, gb := moves[0], moves[1]
	if ga.Namespace != "ns" || ga.Cards != 4 || len(ga.Pods) != 1 {
		t.Errorf("ga=%+v", ga)
	}
	if gb.Cards != 4 || len(gb.Pods) != 2 { // 2+2, two pods from two source nodes
		t.Errorf("gb cards=%d pods=%d, want 4/2", gb.Cards, len(gb.Pods))
	}
	// gb pods are name-sorted: w0 then w1.
	if gb.Pods[0].Name != "w0" || gb.Pods[0].FromNode != "n1" || gb.Pods[1].Name != "w1" {
		t.Errorf("gb pods not sorted: %+v", gb.Pods)
	}
}

func TestSummaryOf(t *testing.T) {
	s := summaryOf(engineframework.Report{FragRateBefore: 0.4, FragRateAfter: 0.2, NodesFreed: 3})
	if s.FragBeforePercent != 40 || s.FragAfterPercent != 20 || s.FreedNodeCount != 3 {
		t.Errorf("summary=%+v", s)
	}
}

func TestNominationsOf(t *testing.T) {
	if nominationsOf(nil, time.Minute) != nil {
		t.Error("nil plan -> nil")
	}
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		{
			Task: &schedapi.TaskInfo{
				Name: "w-0", Namespace: "ns", Job: "ns/g",
				Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"apps.kubernetes.io/pod-index": "0"}}},
			},
			From: "n0", To: "n2",
		},
	}}
	noms := nominationsOf(plan, time.Hour)
	if len(noms) != 1 {
		t.Fatalf("got %d nominations, want 1", len(noms))
	}
	n := noms[0]
	if n.Namespace != "ns" || n.PodGroupName != "g" || n.VictimPodName != "w-0" || n.NodeName != "n2" {
		t.Errorf("nomination=%+v", n)
	}
	if n.Phase != "Pending" {
		t.Errorf("phase=%q, want Pending", n.Phase)
	}
	if n.IdentityLabels["apps.kubernetes.io/pod-index"] != "0" {
		t.Errorf("identityLabels=%v, want pod-index=0", n.IdentityLabels)
	}
	if n.ExpirationTime == nil || !n.ExpirationTime.After(time.Now()) {
		t.Errorf("expirationTime not set in the future: %v", n.ExpirationTime)
	}
}

func TestApplyPlan(t *testing.T) {
	plan := &engineapi.RepackPlan{
		Moves:      []*engineapi.Move{mkMove("a", "ns/g", 3, "n0", "n1")},
		FreedNodes: []string{"n0"},
	}
	report := engineframework.Report{FragRateBefore: 0.5, FragRateAfter: 0.25, NodesFreed: 1}

	// DryRun: plan populated, no nominations.
	dry := &repackv1alpha1.RepackRun{}
	applyPlan(dry, report, plan, gpuResource, false, time.Minute)
	if dry.Status.Plan == nil || dry.Status.Plan.Summary == nil {
		t.Fatal("plan/summary not set")
	}
	if dry.Status.Plan.Summary.MovedCardCount != 3 {
		t.Errorf("movedCardCount=%d, want 3", dry.Status.Plan.Summary.MovedCardCount)
	}
	if len(dry.Status.Plan.FreedNodes) != 1 || dry.Status.Nominations != nil {
		t.Errorf("DryRun should have freedNodes and no nominations")
	}

	// Execute: nominations populated.
	exec := &repackv1alpha1.RepackRun{}
	applyPlan(exec, report, plan, gpuResource, true, time.Minute)
	if len(exec.Status.Nominations) != 1 {
		t.Errorf("Execute should populate nominations, got %d", len(exec.Status.Nominations))
	}
}

func TestTerminalOutcome(t *testing.T) {
	mk := func(condType, reason string) *repackv1alpha1.RepackRun {
		r := &repackv1alpha1.RepackRun{}
		r.Status.Conditions = []metav1.Condition{{Type: condType, Status: metav1.ConditionTrue, Reason: reason}}
		return r
	}
	if got := terminalOutcome(mk(state.CondComplete, state.ReasonExecuted)); got != state.ReasonExecuted {
		t.Errorf("complete outcome=%q, want Executed", got)
	}
	if got := terminalOutcome(mk(state.CondFailed, "ExecuteFailed")); got != "ExecuteFailed" {
		t.Errorf("failed outcome=%q, want ExecuteFailed", got)
	}
	if got := terminalOutcome(&repackv1alpha1.RepackRun{}); got != "Unknown" {
		t.Errorf("no condition outcome=%q, want Unknown", got)
	}
}
