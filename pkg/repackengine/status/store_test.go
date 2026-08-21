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

package status

import (
	"testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

func TestMergeRelocationProgressPreservesControllerPlacement(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod", PlannedNodeName: "n2",
		Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
	}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod", PlannedNodeName: "n2",
		Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
		Placement: repackv1alpha1.PodPlacementStatus{
			Phase: repackv1alpha1.PodPlacementPlaced, SelectedNodeName: "n2", ActualNodeName: "n3",
		},
	}}

	MergeRelocationProgress(desired, latest)
	if desired[0].Placement.Phase != repackv1alpha1.PodPlacementPlaced || desired[0].Placement.ActualNodeName != "n3" {
		t.Fatalf("merged placement=%+v, want controller-owned Placed result", desired[0].Placement)
	}
}

func TestTerminalPhasesDoNotReplaceSiblingTerminalPhases(t *testing.T) {
	if PlacementPhaseAdvances(repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut) {
		t.Fatal("TimedOut must not replace sibling terminal phase Placed")
	}
	if EvictionPhaseAdvances(repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionRejected) {
		t.Fatal("Rejected must not replace sibling terminal phase Accepted")
	}
}
