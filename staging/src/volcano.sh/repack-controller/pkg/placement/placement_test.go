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

package placement

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
)

func TestPodGroupName(t *testing.T) {
	controller := true
	owner := metav1.OwnerReference{UID: types.UID("owner-uid"), Controller: &controller}
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "explicit association wins",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations:     map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "explicit"},
				OwnerReferences: []metav1.OwnerReference{owner},
			}},
			want: "explicit",
		},
		{
			name: "controller owner is deterministic before annotation",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner}}},
			want: "podgroup-owner-uid",
		},
		{
			name: "ownerless pod has no admission-time podgroup",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid")}},
			want: "",
		},
		{
			name: "nil pod has no podgroup",
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PodGroupName(test.pod); got != test.want {
				t.Fatalf("PodGroupName() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOwnerValue(t *testing.T) {
	value := OwnerValue("run", types.UID("uid"))
	if value != "run/uid" {
		t.Fatalf("OwnerValue() = %q", value)
	}
	name, uid, ok := ParseOwner(value)
	if !ok || name != "run" || uid != types.UID("uid") {
		t.Fatalf("ParseOwner() = %q/%q, %v", name, uid, ok)
	}
	if value := OwnerValue("", types.UID("uid")); value != "" {
		t.Fatalf("OwnerValue() with empty name = %q", value)
	}
	if value := OwnerValue("run", ""); value != "" {
		t.Fatalf("OwnerValue() with empty UID = %q", value)
	}
	for _, malformed := range []string{"", "run", "/uid", "run/"} {
		if _, _, ok := ParseOwner(malformed); ok {
			t.Fatalf("malformed owner %q parsed successfully", malformed)
		}
	}
}

func TestActiveForPodGroup(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Nominations: []repackv1alpha1.PodNomination{{
				Namespace: "ns", PodGroupName: "pg", Phase: repackv1alpha1.PodPlacementPrepared,
			}},
		},
	}
	if !ActiveForPodGroup(run, "ns", "pg") {
		t.Fatal("prepared placement must be active")
	}
	run.Status.Nominations[0].Phase = repackv1alpha1.PodPlacementPlaced
	if ActiveForPodGroup(run, "ns", "pg") {
		t.Fatal("placed nomination must not remain active")
	}
	if ActiveForPodGroup(run, "other", "pg") {
		t.Fatal("another namespace must not be active")
	}
}

func TestPlacementAppliesToPodGroupAcceptsRecordedAndWorkloadReplacement(t *testing.T) {
	controller := true
	start := metav1.NewTime(time.Unix(100, 0))
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackRunning,
			StartTime: &start,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
				Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
			}}},
			Nominations: []repackv1alpha1.PodNomination{{
				Namespace: "ns", PodGroupName: "old", Phase: repackv1alpha1.PodPlacementPrepared,
			}},
		},
	}
	replacement := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace:         "ns",
		Name:              "new",
		CreationTimestamp: metav1.NewTime(time.Unix(101, 0)),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	if !PlacementAppliesToPodGroup(run, replacement) {
		t.Fatal("new PodGroup owned by an affected workload must be active before mapping is recorded")
	}

	run.Status.Nominations[0].ReplacementPodGroupName = "new"
	if !ActiveForPodGroup(run, "ns", "new") {
		t.Fatal("recorded replacement PodGroup must be active")
	}

	run.Status.Nominations[0].ReplacementPodGroupName = ""
	unrelated := replacement.DeepCopy()
	unrelated.OwnerReferences[0].Name = "another-model"
	if PlacementAppliesToPodGroup(run, unrelated) {
		t.Fatal("PodGroup owned by another workload must not be active")
	}

	preexisting := replacement.DeepCopy()
	preexisting.CreationTimestamp = metav1.NewTime(time.Unix(99, 0))
	if PlacementAppliesToPodGroup(run, preexisting) {
		t.Fatal("PodGroup created before Execute started must not be inferred as a replacement")
	}
}
