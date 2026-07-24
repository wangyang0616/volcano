/*
Copyright 2025 The Volcano Authors.

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

package mutate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	"volcano.sh/volcano/pkg/webhooks/router"
)

func Test_createPodGroupPatch(t *testing.T) {
	tests := []struct {
		name          string
		podgroup      *schedulingv1beta1.PodGroup
		nsAnnotations map[string]string
		wantPatch     []patchOperation
		wantErr       bool
	}{
		{
			name: "podgroup with non-default queue",
			podgroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: "custom-queue",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
				},
			},
			nsAnnotations: nil,
			wantPatch:     nil,
			wantErr:       false,
		},
		{
			name: "podgroup with default queue and namespace with queue annotation",
			podgroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: schedulingv1beta1.DefaultQueue,
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
				},
			},
			nsAnnotations: map[string]string{
				schedulingv1beta1.QueueNameAnnotationKey: "ns-queue",
			},
			wantPatch: []patchOperation{
				{
					Op:    "add",
					Path:  "/spec/queue",
					Value: "ns-queue",
				},
			},
			wantErr: false,
		},
		{
			name: "podgroup with default queue and namespace without queue annotation",
			podgroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: schedulingv1beta1.DefaultQueue,
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
				},
			},
			nsAnnotations: map[string]string{},
			wantPatch:     nil,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup fake client
			client := fake.NewSimpleClientset()
			if tt.nsAnnotations != nil {
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-ns",
						Annotations: tt.nsAnnotations,
					},
				}
				_, err := client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create test namespace: %v", err)
				}
			}

			config = &router.AdmissionServiceConfig{
				KubeClient: client,
			}

			got, err := createPodGroupPatch(tt.podgroup)
			if (err != nil) != tt.wantErr {
				t.Errorf("createPodGroupPatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantPatch == nil {
				if got != nil {
					t.Errorf("createPodGroupPatch() got = %v, want nil", string(got))
				}
				return
			}

			var gotPatch []patchOperation
			if err := json.Unmarshal(got, &gotPatch); err != nil {
				t.Errorf("Failed to unmarshal patch: %v", err)
				return
			}

			if !reflect.DeepEqual(gotPatch, tt.wantPatch) {
				t.Errorf("createPodGroupPatch() got = %v, want %v", gotPatch, tt.wantPatch)
			}
		})
	}
}

func TestCreateRepackPlacementLeasePatch(t *testing.T) {
	controller := true
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "run",
			UID:    types.UID("run-uid"),
			Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"},
		},
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
				Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
			}}},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "old", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	candidate := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "new",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	previous := config
	defer func() { config = previous }()
	config = &router.AdmissionServiceConfig{VolcanoClient: vcfake.NewSimpleClientset(run)}

	patch := createRepackPlacementLeasePatch(candidate)
	if patch == nil || patch.Path != "/metadata/annotations" {
		t.Fatalf("placement lease patch = %#v", patch)
	}
	annotations, ok := patch.Value.(map[string]string)
	if !ok || annotations[repackv1alpha1.PlacementLeaseAnnotation] != "run/run-uid" {
		t.Fatalf("placement lease annotations = %#v", patch.Value)
	}

	unrelated := candidate.DeepCopy()
	unrelated.OwnerReferences[0].Name = "other"
	if patch := createRepackPlacementLeasePatch(unrelated); patch != nil {
		t.Fatalf("unrelated workload received placement lease: %#v", patch)
	}

	existing := candidate.DeepCopy()
	existing.Annotations = map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "other/uid"}
	if patch := createRepackPlacementLeasePatch(existing); patch != nil {
		t.Fatalf("existing lease must not be overwritten: %#v", patch)
	}
}
