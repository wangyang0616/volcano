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
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
)

func TestHooksForEvictionGracePeriod(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		gracePeriodSeconds *int64
		wantDeleteOptions  bool
		wantGracePeriod    int64
	}{
		{name: "unset preserves pod default", wantDeleteOptions: false},
		{name: "explicit grace period", gracePeriodSeconds: int64Ptr(30), wantDeleteOptions: true, wantGracePeriod: 30},
		{name: "explicit zero", gracePeriodSeconds: int64Ptr(0), wantDeleteOptions: true, wantGracePeriod: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var received *policyv1.Eviction
			client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				evictionAction, ok := action.(k8stesting.CreateAction)
				if !ok {
					t.Fatalf("action=%T, want CreateAction", action)
				}
				var castOK bool
				received, castOK = evictionAction.GetObject().(*policyv1.Eviction)
				if !castOK {
					t.Fatalf("object=%T, want *policyv1.Eviction", evictionAction.GetObject())
				}
				return true, received, nil
			})

			run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{
				Mode:     repackv1alpha1.RepackModeExecute,
				Eviction: &repackv1alpha1.EvictionPolicy{GracePeriodSeconds: testCase.gracePeriodSeconds},
			}}
			move := &engineapi.Move{Task: &schedapi.TaskInfo{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: "workload"}}}}
			if err := hooksFor(run, client).Evict(move); err != nil {
				t.Fatalf("Evict() error = %v", err)
			}
			if received == nil {
				t.Fatal("Evict() did not submit an Eviction")
			}
			if (received.DeleteOptions != nil) != testCase.wantDeleteOptions {
				t.Fatalf("DeleteOptions=%+v, want present=%t", received.DeleteOptions, testCase.wantDeleteOptions)
			}
			if testCase.wantDeleteOptions && (received.DeleteOptions.GracePeriodSeconds == nil || *received.DeleteOptions.GracePeriodSeconds != testCase.wantGracePeriod) {
				t.Fatalf("GracePeriodSeconds=%v, want %d", received.DeleteOptions.GracePeriodSeconds, testCase.wantGracePeriod)
			}
		})
	}
}

func TestHooksForDryRunDoesNotExposeEviction(t *testing.T) {
	if hooks := hooksFor(&repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun}}, fake.NewSimpleClientset()); hooks.Evict != nil {
		t.Fatal("DryRun must not expose an eviction hook")
	}
}

func TestHooksForPreservesVictimNotFoundReason(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "victim")
	})
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	move := &engineapi.Move{Task: &schedapi.TaskInfo{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "victim", Namespace: "workload",
	}}}}
	err := hooksFor(run, client).Evict(move)
	if !errors.Is(err, engineframework.ErrVictimNotFound) {
		t.Fatalf("Evict() error = %v, want ErrVictimNotFound", err)
	}
}

func TestClassifyCascadeDeletionsRetainsOnlySiblingNotFound(t *testing.T) {
	result := &engineframework.CommitResult{
		Evicted: []engineframework.MoveOutcome{{
			Namespace: "ns", PodGroupID: "ns/group-a", PodName: "a-0",
		}},
		Failed: []engineframework.MoveOutcome{
			{Namespace: "ns", PodGroupID: "ns/group-a", PodName: "a-1", VictimPodNotFound: true, Err: "not found"},
			{Namespace: "ns", PodGroupID: "ns/group-b", PodName: "b-0", VictimPodNotFound: true, Err: "not found"},
			{Namespace: "ns", PodGroupID: "ns/group-a", PodName: "a-2", Err: "pdb"},
		},
	}
	classifyCascadeDeletions(result)
	if len(result.CascadeDeleted) != 1 || result.CascadeDeleted[0].PodName != "a-1" {
		t.Fatalf("cascadeDeleted = %+v, want only a-1", result.CascadeDeleted)
	}
	if len(result.Failed) != 2 {
		t.Fatalf("failed = %+v, want unrelated NotFound and PDB rejection", result.Failed)
	}
}

func int64Ptr(value int64) *int64 { return &value }
