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

package api

import "testing"

// Benefit sums freed-unit weights (node=1, hypernode>1), capturing holistic gain.
func TestBenefit_UnitsWeighted(t *testing.T) {
	p := &RepackPlan{FreedUnits: []FreeableUnit{{Weight: 1}, {Weight: 3}}}
	if p.Benefit() != 4 {
		t.Errorf("benefit=%v, want 4", p.Benefit())
	}
}

// With no units recorded, Benefit falls back to the freed-node count (node-only).
func TestBenefit_FallbackNodeCount(t *testing.T) {
	p := &RepackPlan{FreedNodes: []string{"n0", "n1"}}
	if p.Benefit() != 2 {
		t.Errorf("benefit=%v, want 2", p.Benefit())
	}
}

// FragmentationRateDelta = -nodesFreed / providingNodeCount.
func TestFragRateDelta(t *testing.T) {
	p := &RepackPlan{FreedNodes: []string{"n0"}, Before: ResourceFragmentation{ProvidingNodeCount: 10}}
	if got := p.FragmentationRateDelta(); got != -0.1 {
		t.Errorf("delta=%v, want -0.1", got)
	}
	// A zero providing-node count must not divide by zero.
	if got := (&RepackPlan{FreedNodes: []string{"n0"}}).FragmentationRateDelta(); got != 0 {
		t.Errorf("delta with zero providing-node count = %v, want 0", got)
	}
}

// AffectedPodGroups is the sorted distinct set of relocated gangs.
func TestAffectedPodGroups(t *testing.T) {
	p := &RepackPlan{Moves: []*Move{
		{Task: gpuJobTask("a", "g2", 1), From: "n0", To: "n1"},
		{Task: gpuJobTask("b", "g1", 1), From: "n0", To: "n1"},
		{Task: gpuJobTask("c", "g1", 1), From: "n0", To: "n0"}, // no-op, excluded
	}}
	got := p.AffectedPodGroups()
	if len(got) != 2 || string(got[0]) != "g1" || string(got[1]) != "g2" {
		t.Errorf("affected=%v, want [g1 g2]", got)
	}
}
