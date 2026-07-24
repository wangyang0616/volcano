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

package drain

import (
	"fmt"
	"strings"
	"testing"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

func TestFormatCandidateOrderSummaryShowsOnlyThreeBestAndThreeWorst(t *testing.T) {
	ordered := make([]scoredCandidate, 8)
	for index := range ordered {
		nodeName := fmt.Sprintf("node-%d", index)
		ordered[index] = scoredCandidate{
			candidate: candidate{
				unit: api.FreeableUnit{Level: "node", Nodes: []string{nodeName}, Weight: 1},
				key:  nodeName,
			},
			score: framework.CandidateDisruptionScore{
				Total: float64(index),
				Terms: []framework.DisruptionScoreTerm{{
					Name: "movedPods", Weight: 0.1, Raw: float64(index),
					Normalized: float64(index) / 7, Contribution: float64(index) / 70,
				}},
			},
		}
	}

	formatted := formatCandidateOrderSummary(ordered)
	if len(formatted) != 7 {
		t.Fatalf("formatted entries=%d, want 7 (top 3 + marker + bottom 3): %v", len(formatted), formatted)
	}
	for index, want := range []string{"#1 node-0(score=0.000)", "#2 node-1(score=1.000)", "#3 node-2(score=2.000)"} {
		if !strings.Contains(formatted[index], want) {
			t.Errorf("formatted[%d]=%q, want %q", index, formatted[index], want)
		}
	}
	if formatted[3] != "... 2 candidates omitted ..." {
		t.Errorf("middle marker=%q, want omitted count 2", formatted[3])
	}
	for index, want := range []string{"#6 node-5(score=5.000)", "#7 node-6(score=6.000)", "#8 node-7(score=7.000)"} {
		if !strings.Contains(formatted[index+4], want) {
			t.Errorf("formatted[%d]=%q, want %q", index+4, formatted[index+4], want)
		}
	}

	complete := formatCandidateOrderSummary(ordered[:6])
	if len(complete) != 6 {
		t.Fatalf("six candidates must be shown completely: %v", complete)
	}
	for _, entry := range complete {
		if strings.Contains(entry, "omitted") {
			t.Fatalf("six-candidate order unexpectedly truncated: %v", complete)
		}
	}
}

func TestCandidateSelectionReasonExplainsDecision(t *testing.T) {
	scored := func(key string, score, benefit float64) scoredCandidate {
		return scoredCandidate{
			candidate: candidate{key: key, unit: api.FreeableUnit{Weight: benefit}},
			score:     framework.CandidateDisruptionScore{Total: score},
		}
	}
	tests := []struct {
		name    string
		ordered []scoredCandidate
		want    string
	}{
		{
			name:    "only feasible target",
			ordered: []scoredCandidate{scored("node-a", 0, 1)},
			want:    "only feasible drain target",
		},
		{
			name:    "lowest score",
			ordered: []scoredCandidate{scored("node-a", 0.1, 1), scored("node-b", 0.2, 2)},
			want:    "lowest disruption score",
		},
		{
			name:    "benefit tie breaker",
			ordered: []scoredCandidate{scored("node-a", 0.1, 2), scored("node-b", 0.1, 1)},
			want:    "score tie; higher drain benefit",
		},
		{
			name:    "name tie breaker",
			ordered: []scoredCandidate{scored("node-a", 0.1, 1), scored("node-b", 0.1, 1)},
			want:    "score and drain benefit tie; lexical target name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := candidateSelectionReason(test.ordered); got != test.want {
				t.Fatalf("candidateSelectionReason()=%q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatPlanImpactUsesOperatorFriendlyNames(t *testing.T) {
	terms := []framework.DisruptionScoreTerm{
		{Name: "affectedPodGroups", Raw: 1},
		{Name: "movedResource", Raw: 4},
		{Name: "movedPods", Raw: 2},
		{Name: "gangBreaches", Raw: 0},
		{Name: "damagedResource", Raw: 4},
	}
	got := formatPlanImpact(terms, gpu)
	want := "podGroups=1 nvidia.com/gpu=4 pods=2 gangBreaches=0 resourceInAffectedGangs=4"
	if got != want {
		t.Fatalf("formatPlanImpact()=%q, want %q", got, want)
	}
}

func TestFormatScoreContributionsHidesNormalizationMechanics(t *testing.T) {
	terms := []framework.DisruptionScoreTerm{
		{Name: "affectedPodGroups", Contribution: 1},
		{Name: "movedResource", Contribution: 0.125},
	}
	got := formatScoreContributions(terms, gpu)
	want := "podGroups=+1.000 nvidia.com/gpu=+0.125"
	if got != want {
		t.Fatalf("formatScoreContributions()=%q, want %q", got, want)
	}
}
