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

package eviction

import (
	"context"
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
)

func TestExecutorEvictionGracePeriod(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		gracePeriodSeconds *int64
		wantGracePeriod    int64
	}{
		{name: "unset preserves pod default"},
		{name: "explicit grace period", gracePeriodSeconds: int64Ptr(30), wantGracePeriod: 30},
		{name: "explicit zero", gracePeriodSeconds: int64Ptr(0), wantGracePeriod: 0},
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
			move := &engineapi.Move{Task: &schedapi.TaskInfo{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "victim", Namespace: "workload", UID: "victim-uid",
			}}}}
			if err := New(run, client).Evict(context.Background(), move); err != nil {
				t.Fatalf("Evict() error = %v", err)
			}
			if received == nil {
				t.Fatal("Evict() did not submit an Eviction")
			}
			if received.DeleteOptions == nil || received.DeleteOptions.Preconditions == nil ||
				received.DeleteOptions.Preconditions.UID == nil ||
				*received.DeleteOptions.Preconditions.UID != "victim-uid" {
				t.Fatalf("DeleteOptions=%+v, want victim UID precondition", received.DeleteOptions)
			}
			if testCase.gracePeriodSeconds == nil && received.DeleteOptions.GracePeriodSeconds != nil {
				t.Fatalf("GracePeriodSeconds=%v, want nil", received.DeleteOptions.GracePeriodSeconds)
			}
			if testCase.gracePeriodSeconds != nil &&
				(received.DeleteOptions.GracePeriodSeconds == nil ||
					*received.DeleteOptions.GracePeriodSeconds != testCase.wantGracePeriod) {
				t.Fatalf("GracePeriodSeconds=%v, want %d", received.DeleteOptions.GracePeriodSeconds, testCase.wantGracePeriod)
			}
		})
	}
}

func TestNewDryRunDoesNotCreateExecutor(t *testing.T) {
	if executor := New(&repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun}}, fake.NewSimpleClientset()); executor != nil {
		t.Fatal("DryRun must not create an eviction executor")
	}
}

func TestExecutorPreservesVictimNotFoundReason(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "victim")
	})
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	move := &engineapi.Move{Task: &schedapi.TaskInfo{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "victim", Namespace: "workload",
	}}}}
	err := New(run, client).Evict(context.Background(), move)
	if !errors.Is(err, ErrVictimNotFound) {
		t.Fatalf("Evict() error = %v, want ErrVictimNotFound", err)
	}
}

func int64Ptr(value int64) *int64 { return &value }
