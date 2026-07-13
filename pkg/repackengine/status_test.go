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
	"context"
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
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
			// Volcano stores extended resources in milli (1 device = 1000); mirror
			// production so buildStatusMoves's milli->whole conversion is exercised faithfully.
			Resreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: cards * 1000}},
		},
		From: from, To: to,
	}
}

// percentagePoints rounds a 0-1 fraction to a percentage point, clamped to [0,100].
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
		if got := percentagePoints(c.in); got != c.want {
			t.Errorf("percentagePoints(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitJobID(t *testing.T) {
	if ns, n := splitPodGroupID("team-a/gang-1"); ns != "team-a" || n != "gang-1" {
		t.Errorf("split=%q/%q, want team-a/gang-1", ns, n)
	}
	if ns, n := splitPodGroupID("bare"); ns != "" || n != "bare" {
		t.Errorf("split no-slash=%q/%q, want ''/bare", ns, n)
	}
	if ns, n := splitPodGroupID(""); ns != "" || n != "" {
		t.Errorf("split empty=%q/%q, want ''/''", ns, n)
	}
}

func TestFreedNodesOf(t *testing.T) {
	if sortedFreedNodeNames(nil) != nil {
		t.Error("nil plan -> nil")
	}
	got := sortedFreedNodeNames(&engineapi.RepackPlan{FreedNodes: []string{"n2", "n0", "n1"}})
	if len(got) != 3 || got[0] != "n0" || got[1] != "n1" || got[2] != "n2" {
		t.Errorf("freedNodes=%v, want sorted [n0 n1 n2]", got)
	}
}

func TestMovesOf(t *testing.T) {
	// nil plan / no moves.
	if buildStatusMoves(nil, gpuResource) != nil {
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
	moves := buildStatusMoves(plan, gpuResource)
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
	s := buildRepackSummary(engineframework.Report{FragmentationRateBefore: 0.4, FragmentationRateAfter: 0.2, NodesFreed: 3})
	if s.FragBeforePercent != 40 || s.FragAfterPercent != 20 || s.FreedNodeCount != 3 {
		t.Errorf("summary=%+v", s)
	}
}

func TestNominationsOf(t *testing.T) {
	if buildPodNominations(nil, time.Minute) != nil {
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
	noms := buildPodNominations(plan, time.Hour)
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
	report := engineframework.Report{FragmentationRateBefore: 0.5, FragmentationRateAfter: 0.25, NodesFreed: 1}

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

func TestRealizedPlanDropsFailedMovesAndFreedNodes(t *testing.T) {
	a := mkMove("a", "ns/g", 2, "n0", "n2")
	a.Task.Namespace = "ns"
	b := mkMove("b", "ns/g", 2, "n1", "n2")
	b.Task.Namespace = "ns"
	plan := &engineapi.RepackPlan{
		Moves:      []*engineapi.Move{a, b},
		FreedNodes: []string{"n0", "n1"},
		FreedUnits: []engineapi.FreeableUnit{
			{Level: "node", Nodes: []string{"n0"}, Weight: 1},
			{Level: "node", Nodes: []string{"n1"}, Weight: 1},
		},
		Before: engineapi.ResourceFragmentation{Resource: gpuResource, ProvidingNodeCount: 3, OccupiedNodeCount: 2, OptimalOccupiedNodeCount: 1},
	}
	commit := &engineframework.CommitResult{
		Evicted: []engineframework.MoveOutcome{{Namespace: "ns", Task: "a", From: "n0", To: "n2"}},
		Failed:  []engineframework.MoveOutcome{{Namespace: "ns", Task: "b", From: "n1", To: "n2", Err: "pdb"}},
	}
	realized := realizedPlan(plan, commit)
	if len(realized.Moves) != 1 || realized.Moves[0].Task.Name != "a" {
		t.Fatalf("realized moves=%+v, want only a", realized.Moves)
	}
	if len(realized.FreedNodes) != 1 || realized.FreedNodes[0] != "n0" {
		t.Fatalf("realized freed nodes=%v, want [n0]", realized.FreedNodes)
	}
	if len(realized.FreedUnits) != 1 || realized.FreedUnits[0].Nodes[0] != "n0" {
		t.Fatalf("realized freed units=%+v, want n0 unit", realized.FreedUnits)
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

func TestMergeNominationPhasesPreservesControllerOwnedTerminalPhase(t *testing.T) {
	desired := []repackv1alpha1.PodNomination{{Namespace: "ns", PodGroupName: "g", VictimPodName: "p", NodeName: "n", Phase: "Pending"}}
	latest := []repackv1alpha1.PodNomination{{Namespace: "ns", PodGroupName: "g", VictimPodName: "p", NodeName: "n", Phase: "Bound"}}
	mergeNominationPhases(desired, latest)
	if desired[0].Phase != "Bound" {
		t.Fatalf("phase=%q, want Bound", desired[0].Phase)
	}
}

func TestWriteStatusRetriesConflictAndPreservesBoundNomination(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "status-conflict"},
		Status: repackv1alpha1.RepackRunStatus{
			Nominations: []repackv1alpha1.PodNomination{{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", NodeName: "n1", Phase: "Bound",
			}},
		},
	}
	volcanoClient := vcfake.NewSimpleClientset(run)
	updateAttempts := 0
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		updateAttempts++
		if updateAttempts == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				"status-conflict", errors.New("simulated conflict"))
		}
		return false, nil, nil
	})

	desired := run.Status.DeepCopy()
	desired.Nominations[0].Phase = "Pending" // engine's stale view must not undo Bound.
	desired.Phase = repackv1alpha1.RepackSucceeded
	engine := &Engine{volcanoClient: volcanoClient}
	if err := engine.writeStatus(context.Background(), run.Name, desired); err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if updateAttempts != 2 {
		t.Fatalf("status update attempts = %d, want conflict retry", updateAttempts)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != repackv1alpha1.RepackSucceeded {
		t.Errorf("phase = %q, want Succeeded", updated.Status.Phase)
	}
	if updated.Status.Nominations[0].Phase != "Bound" {
		t.Errorf("nomination phase = %q, want controller-owned Bound", updated.Status.Nominations[0].Phase)
	}
}
