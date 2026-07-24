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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const gpuResource = v1.ResourceName("nvidia.com/gpu")

type testPodGroupPlacementPolicies map[schedapi.JobID]bool

func (policies testPodGroupPlacementPolicies) PodGroupUsesSubGroupPolicy(id schedapi.JobID) bool {
	return policies[id]
}

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
	if buildStatusMoves(nil, gpuResource, nil) != nil {
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
	owners := map[string]*repackv1alpha1.WorkloadRef{
		"ns/ga": {APIVersion: "apps/v1", Kind: "Deployment", Name: "trainer"},
	}
	moves := buildStatusMoves(plan, gpuResource, owners)
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
	if ga.Owner == nil || ga.Owner.APIVersion != "apps/v1" || ga.Owner.Kind != "Deployment" || ga.Owner.Name != "trainer" {
		t.Errorf("ga owner=%+v, want apps/v1 Deployment trainer", ga.Owner)
	}
	if gb.Owner != nil {
		t.Errorf("gb owner=%+v, want nil", gb.Owner)
	}
	if gb.Cards != 4 || len(gb.Pods) != 2 { // 2+2, two pods from two source nodes
		t.Errorf("gb cards=%d pods=%d, want 4/2", gb.Cards, len(gb.Pods))
	}
	// gb pods are name-sorted: w0 then w1.
	if gb.Pods[0].Name != "w0" || gb.Pods[0].FromNode != "n1" || gb.Pods[1].Name != "w1" {
		t.Errorf("gb pods not sorted: %+v", gb.Pods)
	}
}

func TestResolveMoveOwners(t *testing.T) {
	controller := true
	client := vcfake.NewSimpleClientset(
		&schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "owned",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "worker", Controller: &controller}},
		}},
		&schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "non-controller",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "helper"}},
		}},
	)
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		mkMove("owned-0", "ns/owned", 1, "n0", "n1"),
		mkMove("plain-0", "ns/non-controller", 1, "n0", "n1"),
		mkMove("missing-0", "ns/missing", 1, "n0", "n1"),
	}}
	owners := (&Engine{volcanoClient: client}).resolveMoveOwners(context.Background(), plan)
	if len(owners) != 1 {
		t.Fatalf("resolved owners=%v, want one controller owner", owners)
	}
	got := owners["ns/owned"]
	if got == nil || got.APIVersion != "apps/v1" || got.Kind != "StatefulSet" || got.Name != "worker" {
		t.Errorf("owner=%+v, want apps/v1 StatefulSet worker", got)
	}
}

func TestSummaryOf(t *testing.T) {
	s := buildRepackSummary(engineframework.Report{FragmentationRateBefore: 0.4, FragmentationRateAfter: 0.2, NodesFreed: 3})
	if s.FragBeforePercent != 40 || s.FragAfterPercent != 20 || s.FreedNodeCount != 3 {
		t.Errorf("summary=%+v", s)
	}
}

func TestRepackSummarySerializesZeroValues(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackSummary{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"fragBeforePercent":0`,
		`"fragAfterPercent":0`,
		`"freedNodeCount":0`,
		`"movedCardCount":0`,
	} {
		if !strings.Contains(string(data), field) {
			t.Errorf("serialized summary %s does not contain %s", data, field)
		}
	}
}

func TestRepackResultSerializesZeroValues(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackResult{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"fragAfterPercent":0`,
		`"freedNodeCount":0`,
		`"movedCardCount":0`,
		`"metricsVerified":false`,
	} {
		if !strings.Contains(string(data), field) {
			t.Errorf("serialized result %s does not contain %s", data, field)
		}
	}
}

func TestRepackResultSerializesVerifiedFreedNodes(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackResult{
		FreedNodeCount:  2,
		FreedNodes:      []string{"node-a", "node-b"},
		MetricsVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"freedNodes":["node-a","node-b"]`) {
		t.Fatalf("serialized result %s does not contain the verified freed-node set", data)
	}
}

func TestBuildResolvedScope(t *testing.T) {
	resource := func(cards int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: float64(cards * 1000)}}
	}
	nodes := []*schedapi.NodeInfo{
		{
			Name:        "n0",
			Node:        &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "selected"}}},
			Allocatable: resource(8),
			Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
				"a": {Job: "ns/a", Resreq: resource(2)},
				"b": {Job: "ns/b", Resreq: resource(2)},
			},
		},
		{
			Name:        "n1",
			Node:        &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "excluded"}}},
			Allocatable: resource(8),
			Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
				"a2": {Job: "ns/a", Resreq: resource(2)},
			},
		},
		{Name: "cpu-only", Allocatable: schedapi.EmptyResource()},
	}
	scope, err := engineframework.NewScopeMatcher(
		&repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/a"}},
			},
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "selected"}},
				},
			},
		},
		func(id schedapi.JobID) (string, labels.Labels, bool) {
			return string(id), labels.Set{}, id == "ns/a" || id == "ns/b"
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := buildResolvedScope(nodes, scope, gpuResource)
	if resolved.NodeCount != 1 || resolved.PodGroupCount != 1 {
		t.Fatalf("resolvedScope = %+v, want nodeCount=1 podGroupCount=1", resolved)
	}
}

func TestStampLifecycleIsIdempotent(t *testing.T) {
	start := time.Unix(100, 0)
	later := start.Add(time.Minute)
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning}}
	stampLifecycle(run, start)
	if run.Status.StartTime == nil || !run.Status.StartTime.Time.Equal(start) || run.Status.CompletionTime != nil {
		t.Fatalf("running lifecycle = %+v", run.Status)
	}
	run.Status.Phase = repackv1alpha1.RepackSucceeded
	stampLifecycle(run, later)
	if run.Status.CompletionTime == nil || !run.Status.CompletionTime.Time.Equal(later) {
		t.Fatalf("completionTime = %v, want %v", run.Status.CompletionTime, later)
	}
	stampLifecycle(run, later.Add(time.Minute))
	if !run.Status.StartTime.Time.Equal(start) || !run.Status.CompletionTime.Time.Equal(later) {
		t.Fatalf("lifecycle timestamps were overwritten: %+v", run.Status)
	}
}

func TestCompletionStatusMessageIncludesOperationalResult(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Status: repackv1alpha1.RepackRunStatus{Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FragBeforePercent: 42,
				FragAfterPercent:  28,
				FreedNodeCount:    2,
				MovedCardCount:    12,
			},
			Moves: []repackv1alpha1.RepackMove{{}, {}, {}},
		}},
	}
	message := completionStatusMessage(run, gpuResource, state.ReasonRepackRecommended)
	for _, want := range []string{"nvidia.com/gpu", "3 PodGroups", "12 cards", "2 nodes", "42% to 28%"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}
}

func TestMarkExecuteNotPerformedPreservesPlanAndClearsExecutionState(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FragBeforePercent: 40,
				FragAfterPercent:  20,
				FreedNodeCount:    2,
				MovedCardCount:    8,
			},
			Moves:      []repackv1alpha1.RepackMove{{PodGroupName: "pg"}},
			FreedNodes: []string{"n0"},
		},
		Result:      &repackv1alpha1.RepackResult{FragAfterPercent: 30, FreedNodeCount: 1, MovedCardCount: 4},
		Relocations: []repackv1alpha1.PodRelocationStatus{{VictimPodName: "pod"}},
	}}
	markExecuteNotPerformed(run)
	summary := run.Status.Plan.Summary
	if summary.FragAfterPercent != 20 || summary.FreedNodeCount != 2 || summary.MovedCardCount != 8 {
		t.Fatalf("plan summary was modified: %+v", summary)
	}
	if len(run.Status.Plan.Moves) != 1 || len(run.Status.Plan.FreedNodes) != 1 {
		t.Fatalf("complete plan was not preserved: %+v", run.Status.Plan)
	}
	if run.Status.Result != nil || len(run.Status.Relocations) != 0 {
		t.Fatalf("execution state was not cleared: %+v", run.Status)
	}
}

func TestBuildPodRelocations(t *testing.T) {
	if relocations, err := buildPodRelocations(nil, time.Minute, nil); err != nil || relocations != nil {
		t.Error("nil plan -> nil")
	}
	pod := &v1.Pod{Spec: v1.PodSpec{NodeSelector: map[string]string{"accelerator": "npu"}}}
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		{
			Task: &schedapi.TaskInfo{
				Name: "w-0", Namespace: "ns", Job: "ns/g",
				Pod: pod,
			},
			From: "n0", To: "n2",
		},
	}}
	relocations, err := buildPodRelocations(plan, time.Hour, testPodGroupPlacementPolicies{
		"ns/g": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(relocations) != 1 {
		t.Fatalf("got %d relocations, want 1", len(relocations))
	}
	relocation := relocations[0]
	if relocation.Namespace != "ns" || relocation.PodGroupName != "g" || relocation.VictimPodName != "w-0" || relocation.PlannedNodeName != "n2" {
		t.Errorf("relocation=%+v", relocation)
	}
	if relocation.Placement.Phase != repackv1alpha1.PodPlacementWaitingForReplacement {
		t.Errorf("phase=%q, want WaitingForReplacement", relocation.Placement.Phase)
	}
	if relocation.SchedulingRequirementsHash == "" {
		t.Error("SubGroup-enabled PodGroup must record schedulingRequirementsHash")
	}
	if relocation.Placement.ExpirationTime == nil || !relocation.Placement.ExpirationTime.After(time.Now()) {
		t.Errorf("expirationTime not set in the future: %v", relocation.Placement.ExpirationTime)
	}

	homogeneous, err := buildPodRelocations(plan, time.Hour, testPodGroupPlacementPolicies{
		"ns/g": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if homogeneous[0].SchedulingRequirementsHash != "" {
		t.Errorf("homogeneous PodGroup hash = %q, want empty", homogeneous[0].SchedulingRequirementsHash)
	}

	missingVictimPod := &engineapi.RepackPlan{Moves: []*engineapi.Move{{
		Task: &schedapi.TaskInfo{Name: "missing", Namespace: "ns", Job: "ns/g"},
		From: "n0", To: "n2",
	}}}
	if _, err := buildPodRelocations(missingVictimPod, time.Hour, testPodGroupPlacementPolicies{
		"ns/g": true,
	}); err == nil {
		t.Fatal("SubGroup placement without a victim Pod must fail before eviction")
	}
}

func TestApplyPlan(t *testing.T) {
	plan := &engineapi.RepackPlan{
		Moves:      []*engineapi.Move{mkMove("a", "ns/g", 3, "n0", "n1")},
		FreedNodes: []string{"n0"},
	}
	report := engineframework.Report{FragmentationRateBefore: 0.5, FragmentationRateAfter: 0.25, NodesFreed: 1}

	// DryRun: plan populated, no relocation execution records.
	dry := &repackv1alpha1.RepackRun{}
	resolved := &repackv1alpha1.ResolvedScope{NodeCount: 3, PodGroupCount: 1}
	applyPlan(dry, report, plan, gpuResource, nil, resolved)
	if dry.Status.Plan == nil || dry.Status.Plan.Summary == nil {
		t.Fatal("plan/summary not set")
	}
	if dry.Status.Plan.Summary.MovedCardCount != 3 {
		t.Errorf("movedCardCount=%d, want 3", dry.Status.Plan.Summary.MovedCardCount)
	}
	if len(dry.Status.Plan.FreedNodes) != 1 || dry.Status.Relocations != nil {
		t.Errorf("DryRun should have freedNodes and no relocations")
	}
	if dry.Status.Plan.Summary.ResolvedScope == nil ||
		dry.Status.Plan.Summary.ResolvedScope.NodeCount != 3 ||
		dry.Status.Plan.Summary.ResolvedScope.PodGroupCount != 1 {
		t.Errorf("resolvedScope=%+v, want 3 nodes/1 PodGroup", dry.Status.Plan.Summary.ResolvedScope)
	}

	// Execute preparation is explicit and does not alter the plan.
	exec := &repackv1alpha1.RepackRun{}
	applyPlan(exec, report, plan, gpuResource, nil, resolved)
	if err := prepareExecuteRelocations(exec, plan, time.Minute, testPodGroupPlacementPolicies{}); err != nil {
		t.Fatal(err)
	}
	if len(exec.Status.Relocations) != 1 {
		t.Errorf("Execute should populate relocations, got %d", len(exec.Status.Relocations))
	}
}

func TestRealizedFreedNodeNamesRequiresEveryPlannedVictim(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			FreedNodes: []string{"n0", "n1"},
			Moves: []repackv1alpha1.RepackMove{
				{Namespace: "ns", PodGroupName: "a", Pods: []repackv1alpha1.PodMove{
					{Name: "a-0", FromNode: "n0", ToNode: "n2"},
					{Name: "a-1", FromNode: "n0", ToNode: "n2"},
				}},
				{Namespace: "ns", PodGroupName: "b", Pods: []repackv1alpha1.PodMove{
					{Name: "b-0", FromNode: "n1", ToNode: "n2"},
				}},
			},
		},
		Relocations: []repackv1alpha1.PodRelocationStatus{
			{Namespace: "ns", PodGroupName: "a", VictimPodName: "a-0", PlannedNodeName: "n2"},
			{Namespace: "ns", PodGroupName: "b", VictimPodName: "b-0", PlannedNodeName: "n2"},
		},
	}}
	got := realizedFreedNodeNames(run)
	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("accepted freed nodes = %v, want [n1]", got)
	}
}

func TestTerminalOutcome(t *testing.T) {
	mk := func(condType, reason string) *repackv1alpha1.RepackRun {
		r := &repackv1alpha1.RepackRun{}
		r.Status.Conditions = []metav1.Condition{{Type: condType, Status: metav1.ConditionTrue, Reason: reason}}
		return r
	}
	if got := terminalOutcome(mk(state.CondComplete, state.ReasonExecutionCompleted)); got != state.ReasonExecutionCompleted {
		t.Errorf("complete outcome=%q, want Executed", got)
	}
	if got := terminalOutcome(mk(state.CondFailed, "ExecuteFailed")); got != "ExecuteFailed" {
		t.Errorf("failed outcome=%q, want ExecuteFailed", got)
	}
	if got := terminalOutcome(&repackv1alpha1.RepackRun{}); got != "Unknown" {
		t.Errorf("no condition outcome=%q, want Unknown", got)
	}
}

func TestMergeRelocationProgressPreservesControllerOwnedPlacementPhase(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection,
			ReplacementPodName: "replacement", ReplacementPodUID: "replacement-uid"},
	}}
	mergeRelocationProgress(desired, latest)
	if desired[0].Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
		t.Fatalf("phase=%q, want Gated", desired[0].Placement.Phase)
	}
	if desired[0].Placement.ReplacementPodName != "replacement" || desired[0].Placement.ReplacementPodUID != "replacement-uid" {
		t.Fatalf("replacement identity=%q/%q, want replacement/replacement-uid", desired[0].Placement.ReplacementPodName, desired[0].Placement.ReplacementPodUID)
	}
}

func TestMergeRelocationProgressDoesNotOverwriteTerminalPlacementWithOlderObservation(t *testing.T) {
	for _, olderPhase := range []repackv1alpha1.PodPlacementPhase{
		repackv1alpha1.PodPlacementWaitingForReplacement,
		repackv1alpha1.PodPlacementWaitingForNodeSelection,
		repackv1alpha1.PodPlacementNominated,
	} {
		t.Run(string(olderPhase), func(t *testing.T) {
			desired := []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
				Placement: repackv1alpha1.PodPlacementStatus{
					Phase: repackv1alpha1.PodPlacementTimedOut,
				},
			}}
			latest := []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
				Placement: repackv1alpha1.PodPlacementStatus{
					Phase:              olderPhase,
					ReplacementPodName: "replacement",
					ReplacementPodUID:  "replacement-uid",
				},
			}}

			mergeRelocationProgress(desired, latest)
			if desired[0].Placement.Phase != repackv1alpha1.PodPlacementTimedOut {
				t.Fatalf("phase=%q, want TimedOut", desired[0].Placement.Phase)
			}
		})
	}
}

func TestMergeRelocationProgressPreservesPodGroupReplacement(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "old", VictimPodName: "pod", PlannedNodeName: "node", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	}}
	latest := append([]repackv1alpha1.PodRelocationStatus(nil), desired...)
	latest[0].ReplacementPodGroupName = "new"

	mergeRelocationProgress(desired, latest)
	if desired[0].ReplacementPodGroupName != "new" {
		t.Fatalf("replacementPodGroupName = %q, want new", desired[0].ReplacementPodGroupName)
	}
}

func TestWriteStatusRetriesConflictAndPreservesBoundNomination(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "status-conflict"},
		Status: repackv1alpha1.RepackRunStatus{
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced},
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
	desired.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementWaitingForReplacement // engine's stale view must not undo Placed.
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
	if updated.Status.Relocations[0].Placement.Phase != repackv1alpha1.PodPlacementPlaced {
		t.Errorf("placement phase = %q, want controller-owned Placed", updated.Status.Relocations[0].Placement.Phase)
	}
}

func TestUpdateStatusTerminalPersistsMessageAndCompletionTime(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-time.Minute))
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-status"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackSucceeded,
			Message:   "operator-readable result",
			StartTime: &startTime,
			Conditions: []metav1.Condition{{
				Type: state.CondComplete, Status: metav1.ConditionTrue, Reason: state.ReasonNoFragmentation,
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	recorder := record.NewFakeRecorder(10)
	engine := &Engine{volcanoClient: client, recorder: recorder}
	if err := engine.updateStatusTerminal(context.Background(), run); err != nil {
		t.Fatalf("updateStatusTerminal() error = %v", err)
	}
	updated, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.Message != "operator-readable result" {
		t.Errorf("message=%q, want operator-readable result", updated.Status.Message)
	}
	if updated.Status.CompletionTime == nil {
		t.Fatal("completionTime was not persisted")
	}
	if updated.Status.CompletionTime.Time.Before(startTime.Time) {
		t.Errorf("completionTime=%v precedes startTime=%v", updated.Status.CompletionTime, startTime)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, state.ReasonNoFragmentation) ||
			!strings.Contains(event, "operator-readable result") {
			t.Fatalf("terminal event = %q, want reason and operator-readable message", event)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal RepackRun event was not recorded")
	}
}

func TestUpdateStatusTerminalYieldsAfterBoundedFailures(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-status-retry"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackSucceeded,
			Conditions: []metav1.Condition{{
				Type: state.CondComplete, Status: metav1.ConditionTrue, Reason: state.ReasonNoFragmentation,
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	failWrites := true
	updateAttempts := 0
	client.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		updateAttempts++
		if failWrites {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				run.Name, errors.New("simulated persistent RBAC failure"))
		}
		return false, nil, nil
	})
	engine := &Engine{
		volcanoClient:           client,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}

	err := engine.updateStatusTerminal(context.Background(), run.DeepCopy())
	if !isTerminalStatusPersistenceError(err) {
		t.Fatalf("updateStatusTerminal() error = %v, want terminal persistence error", err)
	}
	if updateAttempts != terminalStatusWriteAttempts {
		t.Fatalf("status update attempts = %d, want bounded %d", updateAttempts, terminalStatusWriteAttempts)
	}
	if reconcileErrorConsumesRetryBudget(err) {
		t.Fatal("terminal persistence error must yield and requeue without consuming poison-pill budget")
	}
	if _, found := engine.pendingTerminalStatus(run.Name); !found {
		t.Fatal("terminal projection was not retained for the queued retry")
	}

	failWrites = false
	desired, found := engine.pendingTerminalStatus(run.Name)
	if !found {
		t.Fatal("pending terminal projection disappeared")
	}
	retryRun := run.DeepCopy()
	desired.DeepCopyInto(&retryRun.Status)
	if err := engine.updateStatusTerminal(context.Background(), retryRun); err != nil {
		t.Fatalf("terminal status retry failed: %v", err)
	}
	if _, found := engine.pendingTerminalStatus(run.Name); found {
		t.Fatal("terminal projection was not cleared after persistence succeeded")
	}
}
