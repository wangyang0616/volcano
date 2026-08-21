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

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
)

func TestCandidatesRequireConcreteReplacement(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{Relocations: []repackv1alpha1.PodRelocationStatus{
		{VictimPodName: "ready", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection, ReplacementPodName: "replacement", ReplacementPodUID: "uid"}},
		{VictimPodName: "missing-uid", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection, ReplacementPodName: "replacement"}},
	}}}
	candidates := Candidates(run)
	if len(candidates) != 1 || candidates[0].VictimPodName != "ready" {
		t.Fatalf("Candidates()=%v, want only concrete replacement", candidates)
	}
}

func TestEvaluateTerminalRequiresVerifiedBenefit(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan:   &repackv1alpha1.RepackPlan{FreedNodes: []string{"n1"}},
		Result: &repackv1alpha1.RepackResult{FreedNodes: []string{"n1"}, MetricsVerified: false},
	}}
	decision := EvaluateTerminal(run, false)
	if decision.Succeeded || decision.Reason != state.ReasonResultVerificationFailed {
		t.Fatalf("decision=%+v, want result verification failure", decision)
	}
}
