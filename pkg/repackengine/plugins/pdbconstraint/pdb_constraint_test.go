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

package pdbconstraint

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const testResource v1.ResourceName = "example.com/accelerator"

type pdbSnapshot struct {
	nodes []*schedapi.NodeInfo
	pdbs  []*policyv1.PodDisruptionBudget
	err   error
}

func (s *pdbSnapshot) Nodes() []*schedapi.NodeInfo { return s.nodes }
func (*pdbSnapshot) NodeInScope(*schedapi.NodeInfo) bool {
	return true
}
func (*pdbSnapshot) PodGroupView(schedapi.JobID) engineapi.PodGroupView {
	return engineapi.PodGroupView{}
}
func (*pdbSnapshot) FeasibleRelocation(
	context.Context,
	[]*engineapi.Move,
	[]*schedapi.TaskInfo,
	[]*schedapi.NodeInfo,
) ([]*engineapi.Move, bool) {
	return nil, false
}
func (s *pdbSnapshot) ListPodDisruptionBudgets() ([]*policyv1.PodDisruptionBudget, error) {
	return s.pdbs, s.err
}

type snapshotWithoutPDBReader struct{ nodes []*schedapi.NodeInfo }

func (s *snapshotWithoutPDBReader) Nodes() []*schedapi.NodeInfo { return s.nodes }
func (*snapshotWithoutPDBReader) NodeInScope(*schedapi.NodeInfo) bool {
	return true
}
func (*snapshotWithoutPDBReader) PodGroupView(schedapi.JobID) engineapi.PodGroupView {
	return engineapi.PodGroupView{}
}
func (*snapshotWithoutPDBReader) FeasibleRelocation(
	context.Context,
	[]*engineapi.Move,
	[]*schedapi.TaskInfo,
	[]*schedapi.NodeInfo,
) ([]*engineapi.Move, bool) {
	return nil, false
}

func TestIsZeroDisruptionPDB(t *testing.T) {
	tests := []struct {
		name           string
		generation     int64
		observed       int64
		expected       int32
		desiredHealthy int32
		zeroDisruption bool
		syncFailed     bool
	}{
		{name: "desired equals expected", generation: 1, observed: 1, expected: 4, desiredHealthy: 4, zeroDisruption: true},
		{name: "desired exceeds expected", generation: 1, observed: 1, expected: 4, desiredHealthy: 5, zeroDisruption: true},
		{name: "one theoretical disruption", generation: 1, observed: 1, expected: 4, desiredHealthy: 3},
		{name: "stale status", generation: 2, observed: 1, expected: 4, desiredHealthy: 4},
		{name: "controller sync failed", generation: 1, observed: 1, expected: 4, desiredHealthy: 4, syncFailed: true},
		{name: "no expected pods", generation: 1, observed: 1, expected: 0, desiredHealthy: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pdb := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Generation: test.generation},
				Status: policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: test.observed,
					ExpectedPods:       test.expected,
					DesiredHealthy:     test.desiredHealthy,
				},
			}
			if test.syncFailed {
				pdb.Status.Conditions = []metav1.Condition{{
					Type: policyv1.DisruptionAllowedCondition, Status: metav1.ConditionFalse, Reason: policyv1.SyncFailedReason,
				}}
			}
			if got := isZeroDisruptionPDB(pdb); got != test.zeroDisruption {
				t.Fatalf("isZeroDisruptionPDB() = %t, want %t", got, test.zeroDisruption)
			}
		})
	}
}

func TestPluginBlocksOnlyPodsMatchingFreshZeroDisruptionPDB(t *testing.T) {
	blocked := testTask("blocked", map[string]string{"app": "protected"}, true)
	unmatched := testTask("unmatched", map[string]string{"app": "other"}, true)
	transient := testTask("transient", map[string]string{"app": "rolling"}, true)
	snapshot := &pdbSnapshot{
		nodes: []*schedapi.NodeInfo{testNode(blocked, unmatched, transient)},
		pdbs: []*policyv1.PodDisruptionBudget{
			zeroDisruptionPDB("strict", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "protected"}}),
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "temporarily-full", Generation: 1},
				Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "rolling"}}},
				Status: policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1, ExpectedPods: 2, DesiredHealthy: 1, DisruptionsAllowed: 0,
				},
			},
		},
	}
	ssn := openTestSession(snapshot)
	defer framework.CloseSession(ssn)
	movable := ssn.Movable()

	if movable(blocked) {
		t.Fatal("Pod matched by a fresh zero-disruption PDB must not be movable")
	}
	if !movable(unmatched) {
		t.Fatal("Pod outside the strict PDB selector must remain movable")
	}
	if !movable(transient) {
		t.Fatal("transient disruptionsAllowed=0 must remain eligible for execution retry")
	}
}

func TestPluginHonorsPolicyV1SelectorAndUnhealthySemantics(t *testing.T) {
	alwaysAllow := policyv1.AlwaysAllow
	ready := testTask("ready", nil, true)
	unready := testTask("unready", nil, false)
	alreadyDisrupted := testTask("disrupted", nil, true)
	strict := zeroDisruptionPDB("all-pods", &metav1.LabelSelector{})
	strict.Spec.UnhealthyPodEvictionPolicy = &alwaysAllow
	strict.Status.DisruptedPods = map[string]metav1.Time{alreadyDisrupted.Name: metav1.Now()}

	ssn := openTestSession(&pdbSnapshot{
		nodes: []*schedapi.NodeInfo{testNode(ready, unready, alreadyDisrupted)},
		pdbs:  []*policyv1.PodDisruptionBudget{strict},
	})
	defer framework.CloseSession(ssn)
	movable := ssn.Movable()

	if movable(ready) {
		t.Fatal("an empty policy/v1 selector must protect every Ready Pod in the namespace")
	}
	if !movable(unready) {
		t.Fatal("an unready Pod under AlwaysAllow must not be statically blocked")
	}
	if !movable(alreadyDisrupted) {
		t.Fatal("a Pod already recorded in disruptedPods must not be blocked again")
	}
}

func TestBlockingConstraintMatchesEvictionAPIExemptions(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name           string
		phase          v1.PodPhase
		ready          bool
		terminating    bool
		policy         *policyv1.UnhealthyPodEvictionPolicyType
		currentHealthy int32
		desiredHealthy int32
		wantBlocked    bool
	}{
		{name: "ready pod remains protected", phase: v1.PodRunning, ready: true, currentHealthy: 2, desiredHealthy: 2, wantBlocked: true},
		{name: "pending pod bypasses PDB", phase: v1.PodPending, currentHealthy: 2, desiredHealthy: 2},
		{name: "succeeded pod bypasses PDB", phase: v1.PodSucceeded, currentHealthy: 2, desiredHealthy: 2},
		{name: "failed pod bypasses PDB", phase: v1.PodFailed, currentHealthy: 2, desiredHealthy: 2},
		{name: "terminating pod bypasses PDB", phase: v1.PodRunning, ready: true, terminating: true, currentHealthy: 2, desiredHealthy: 2},
		{name: "AlwaysAllow permits unready pod", phase: v1.PodRunning, policy: unhealthyPolicy(policyv1.AlwaysAllow), desiredHealthy: 2},
		{name: "IfHealthyBudget permits unready pod when workload is healthy", phase: v1.PodRunning, currentHealthy: 2, desiredHealthy: 2},
		{name: "IfHealthyBudget protects unready pod when workload is disrupted", phase: v1.PodRunning, currentHealthy: 1, desiredHealthy: 2, wantBlocked: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := testTask("victim", nil, test.ready)
			task.Pod.Status.Phase = test.phase
			if test.terminating {
				task.Pod.DeletionTimestamp = &now
			}
			constraint := compiledConstraint{
				selector:       labels.Everything(),
				currentHealthy: test.currentHealthy,
				desiredHealthy: test.desiredHealthy,
			}
			if test.policy != nil {
				constraint.unhealthyPodEvictionPolicy = *test.policy
			}
			if _, blocked := blockingConstraint(task.Pod, []compiledConstraint{constraint}); blocked != test.wantBlocked {
				t.Fatalf("blockingConstraint() blocked = %t, want %t", blocked, test.wantBlocked)
			}
		})
	}
}

func TestPluginTreatsNilSelectorAndUnavailableReaderAsFailOpen(t *testing.T) {
	task := testTask("victim", map[string]string{"app": "protected"}, true)
	for name, snapshot := range map[string]framework.Snapshot{
		"nil selector": &pdbSnapshot{
			nodes: []*schedapi.NodeInfo{testNode(task)},
			pdbs:  []*policyv1.PodDisruptionBudget{zeroDisruptionPDB("nil-selector", nil)},
		},
		"list failure": &pdbSnapshot{
			nodes: []*schedapi.NodeInfo{testNode(task)},
			err:   errors.New("cache unavailable"),
		},
		"invalid selector": &pdbSnapshot{
			nodes: []*schedapi.NodeInfo{testNode(task)},
			pdbs: []*policyv1.PodDisruptionBudget{zeroDisruptionPDB("invalid-selector", &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "invalid"}},
			})},
		},
		"reader unavailable": &snapshotWithoutPDBReader{nodes: []*schedapi.NodeInfo{testNode(task)}},
	} {
		t.Run(name, func(t *testing.T) {
			ssn := openTestSession(snapshot)
			defer framework.CloseSession(ssn)
			if !ssn.Movable()(task) {
				t.Fatal("pdbconstraint must fail open when it cannot establish a deterministic match")
			}
		})
	}
}

func TestPluginRejectsArguments(t *testing.T) {
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{}); err != nil {
		t.Fatalf("empty arguments rejected: %v", err)
	}
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{"mode": "dynamic"}); err == nil {
		t.Fatal("unsupported pdbconstraint argument must be rejected")
	}
}

func openTestSession(snapshot framework.Snapshot) *framework.Session {
	return framework.OpenSession(framework.SessionConfig{
		Snapshot: snapshot,
		Resource: testResource,
		Run:      &repackv1alpha1.RepackRun{ObjectMeta: metav1.ObjectMeta{Name: "test-run"}},
	}, framework.PluginOptions(Name))
}

func zeroDisruptionPDB(name string, selector *metav1.LabelSelector) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Generation: 1},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selector},
		Status: policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: 1,
			ExpectedPods:       2,
			DesiredHealthy:     2,
		},
	}
}

func unhealthyPolicy(policy policyv1.UnhealthyPodEvictionPolicyType) *policyv1.UnhealthyPodEvictionPolicyType {
	return &policy
}

func testTask(name string, podLabels map[string]string, ready bool) *schedapi.TaskInfo {
	resource := &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: 1}}
	conditionStatus := v1.ConditionFalse
	if ready {
		conditionStatus = v1.ConditionTrue
	}
	return &schedapi.TaskInfo{
		UID:        schedapi.TaskID(name),
		Job:        "ns/group",
		Name:       name,
		Namespace:  "ns",
		InitResreq: resource,
		TransactionContext: schedapi.TransactionContext{
			NodeName: "node-a",
		},
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: podLabels},
			Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{{
				Type: v1.PodReady, Status: conditionStatus,
			}}},
		},
	}
}

func testNode(tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	node := &schedapi.NodeInfo{Name: "node-a", Tasks: make(map[schedapi.TaskID]*schedapi.TaskInfo)}
	for _, task := range tasks {
		node.Tasks[task.UID] = task
	}
	return node
}
