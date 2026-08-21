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

package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/repack-controller/pkg/state"
)

func TestExecutePreparedEvictionsRecoversAcceptedRequestAfterStatusFailure(t *testing.T) {
	const (
		runName      = "run"
		namespace    = "ns"
		podGroupName = "pg"
		podName      = "victim"
		podUID       = types.UID("victim-uid")
	)
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: runName, UID: "run-uid"},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: "example.com/accelerator"}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Conditions: []metav1.Condition{{
				Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonEvicting,
			}},
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 50, FreedNodeCount: 1, MovedCardCount: 2},
				FreedNodes: []string{"node-a"},
				Moves: []repackv1alpha1.RepackMove{{
					Namespace: namespace, PodGroupName: podGroupName, Cards: 2,
					Pods: []repackv1alpha1.PodMove{{
						Name: podName, FromNode: "node-a", ToNode: "node-b", Cards: 2,
					}},
				}},
			},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: namespace, PodGroupName: podGroupName,
				VictimPodName: podName, VictimPodUID: podUID,
				PlannedNodeName: "node-b", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podGroupName,
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid",
		},
	}}
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy(), podGroup)
	statusUpdates := 0
	failAcceptedStatusOnce := true
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		updated := action.(k8stesting.UpdateAction).GetObject().(*repackv1alpha1.RepackRun)
		if failAcceptedStatusOnce &&
			updated.Status.Relocations[0].Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			failAcceptedStatusOnce = false
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				runName, errors.New("simulated status outage after eviction acceptance"))
		}
		return false, nil, nil
	})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podName, UID: podUID,
	}}
	kubeClient := kubefake.NewSimpleClientset(pod)
	evictionCalls := 0
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalls++
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		current, err := kubeClient.Tracker().Get(v1.SchemeGroupVersion.WithResource("pods"), namespace, podName)
		if err != nil {
			return true, nil, err
		}
		terminating := current.(*v1.Pod).DeepCopy()
		now := metav1.Now()
		terminating.DeletionTimestamp = &now
		if err := kubeClient.Tracker().Update(v1.SchemeGroupVersion.WithResource("pods"), terminating, namespace); err != nil {
			return true, nil, err
		}
		return true, eviction, nil
	})

	engine := &Engine{
		volcanoClient: volcanoClient,
		workQueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
		recorder:                record.NewFakeRecorder(20),
		now:                     time.Now,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
	t.Cleanup(engine.workQueue.ShutDown)

	err := engine.executePreparedEvictionsWithClient(
		context.Background(), run.DeepCopy(), run.Generation, "example.com/accelerator", kubeClient)
	if err == nil {
		t.Fatal("first execution unexpectedly succeeded despite simulated accepted-status outage")
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls = %d, want 1", evictionCalls)
	}
	persisted, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionInProgress {
		t.Fatalf("phase after failed accepted write = %q, want InProgress", got)
	}

	if err := engine.executePreparedEvictionsWithClient(
		context.Background(), persisted.DeepCopy(), persisted.Generation,
		"example.com/accelerator", kubeClient); err != nil {
		t.Fatalf("recovery execution failed: %v", err)
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls after recovery = %d, want no replay", evictionCalls)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("recovered eviction phase = %q, want Accepted", got)
	}
	if updated.Status.Result == nil || updated.Status.Result.MovedCardCount != 2 {
		t.Fatalf("result = %#v, want movedCardCount=2", updated.Status.Result)
	}
	if !hasProgressingReason(updated, state.ReasonReconcilingPlacements) {
		t.Fatalf("conditions = %#v, want ReconcilingPlacements", updated.Status.Conditions)
	}
	if statusUpdates < 4 {
		t.Fatalf("status updates = %d, want durable intent, outcome, result and placement barriers", statusUpdates)
	}
}

func TestClassifyMissingVictimsRequiresAcceptedSibling(t *testing.T) {
	relocations := []repackv1alpha1.PodRelocationStatus{
		{Namespace: "ns", PodGroupName: "group-a", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
		{Namespace: "ns", PodGroupName: "group-a"},
		{Namespace: "ns", PodGroupName: "group-b"},
	}
	if !classifyMissingVictims(relocations, map[int]string{
		1: "Victim Pod was not found.",
		2: "Victim Pod was not found.",
	}) {
		t.Fatal("classification unexpectedly reported no change")
	}
	if got := relocations[1].Eviction.Phase; got != repackv1alpha1.PodEvictionIndirectlyRemoved {
		t.Fatalf("accepted sibling phase = %q, want IndirectlyRemoved", got)
	}
	if got := relocations[2].Eviction.Phase; got != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("unrelated missing victim phase = %q, want Rejected", got)
	}
}

func hasProgressingReason(run *repackv1alpha1.RepackRun, reason string) bool {
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == reason {
			return true
		}
	}
	return false
}
