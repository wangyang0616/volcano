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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func TestPlacementReceiversExcludeFreedNodesAndRequireImmediateIdleCapacity(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	resourceOf := func(quantity int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resource: float64(quantity)}}
	}
	nodes := []*schedapi.NodeInfo{
		{Name: "planned", Idle: resourceOf(2)},
		{Name: "freeing", Idle: resourceOf(8)},
		{Name: "too-small", Idle: resourceOf(1)},
		{Name: "alternative", Idle: resourceOf(4)},
	}
	task := &schedapi.TaskInfo{InitResreq: resourceOf(2)}

	receivers := placementReceivers(nodes, []string{"freeing"}, "planned", task)
	if len(receivers) != 2 {
		t.Fatalf("receiver count = %d, want 2", len(receivers))
	}
	if receivers[0].Name != "planned" || receivers[1].Name != "alternative" {
		t.Errorf("receiver order = [%s, %s], want [planned, alternative]", receivers[0].Name, receivers[1].Name)
	}
}

func TestPlacementCandidatesRequireConcreteGatedReplacement(t *testing.T) {
	run := &repackv1alpha1.RepackRun{}
	run.Status.Nominations = []repackv1alpha1.PodNomination{
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "prepared", NodeName: "n1", Phase: repackv1alpha1.PodPlacementPrepared},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "selected", NodeName: "n1", ReplacementPodName: "p-selected", ReplacementPodUID: "uid", SelectedNodeName: "n2", Phase: repackv1alpha1.PodPlacementGated},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "awaiting", NodeName: "n1", ReplacementPodName: "p-awaiting", ReplacementPodUID: "uid", Phase: repackv1alpha1.PodPlacementAwaitingCapacity},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "gated", NodeName: "n1", ReplacementPodName: "p-gated", ReplacementPodUID: "uid", Phase: repackv1alpha1.PodPlacementGated},
	}

	candidates := placementCandidates(run)
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	if candidates[0].VictimPodName != "awaiting" || candidates[1].VictimPodName != "gated" {
		t.Errorf("candidates = [%s, %s], want deterministic [awaiting, gated]", candidates[0].VictimPodName, candidates[1].VictimPodName)
	}
}

func TestPreparePlacementLeaseReclaimsTerminalLease(t *testing.T) {
	oldRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "old", UID: types.UID("old-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:       repackv1alpha1.RepackFailed,
			Nominations: []repackv1alpha1.PodNomination{{Namespace: "ns", PodGroupName: "pg", Phase: repackv1alpha1.PodPlacementExpired}},
		},
	}
	newRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "new", UID: types.UID("new-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status:     repackv1alpha1.RepackRunStatus{Nominations: []repackv1alpha1.PodNomination{{Namespace: "ns", PodGroupName: "pg", Phase: repackv1alpha1.PodPlacementPrepared}}},
	}
	pg := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pg", Annotations: map[string]string{
		repackv1alpha1.PlacementLeaseAnnotation: "old/old-uid",
	}}}
	client := vcfake.NewSimpleClientset(oldRun, newRun, pg)
	e := &Engine{volcanoClient: client}

	if err := e.preparePlacementLeases(context.Background(), newRun); err != nil {
		t.Fatalf("prepare placement lease: %v", err)
	}
	updated, err := client.SchedulingV1beta1().PodGroups("ns").Get(context.Background(), "pg", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := updated.Annotations[repackv1alpha1.PlacementLeaseAnnotation], "new/new-uid"; got != want {
		t.Fatalf("lease = %q, want %q", got, want)
	}
}
