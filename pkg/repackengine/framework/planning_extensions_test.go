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

package framework

import (
	"cmp"
	"fmt"
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

func TestCandidateAdmissibleStopsAtFirstPluginVeto(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	called := []string{}
	ssn.AddCandidateFilterFn("allow", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "allow")
		return nil
	})
	ssn.AddCandidateFilterFn("budget", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "budget")
		return &CandidateFilterResult{Reason: "max_resource"}
	})
	ssn.AddCandidateFilterFn("must-not-run", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "must-not-run")
		return nil
	})

	result := ssn.CandidateAdmissible(&PlanningCandidate{})
	if result == nil || result.Reason != "max_resource" {
		t.Fatalf("result=%+v, want max_resource veto", result)
	}
	if got := fmt.Sprint(called); got != "[allow budget]" {
		t.Fatalf("callbacks=%s, want configured order with first-veto stop", got)
	}
}

func TestOrderVictimsComposesComparators(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddVictimOrderFn("resource", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(api.Scalar(right.InitResreq, gpu), api.Scalar(left.InitResreq, gpu))
	})
	ssn.AddVictimOrderFn("name", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(left.Name, right.Name)
	})

	ordered := ssn.OrderVictims([]*schedapi.TaskInfo{
		task("b", "g", 1), task("c", "g", 2), task("a", "g", 1),
	})
	if got := fmt.Sprint([]string{ordered[0].Name, ordered[1].Name, ordered[2].Name}); got != "[c a b]" {
		t.Fatalf("victim order=%s, want [c a b]", got)
	}
}

func TestOrderReceiversUsesPriorityAndEvaluatesEachRankOnce(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	stabilityCalls, fitCalls := 0, 0
	ssn.AddReceiverRankFn("bestFit", 30, func(_ *api.PlanContext, _ *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank {
		fitCalls++
		return ReceiverRank{-receiver.AvailableResource}
	})
	ssn.AddReceiverRankFn("stability", 10, func(_ *api.PlanContext, _ *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank {
		stabilityCalls++
		if receiver.StaysOccupied {
			return ReceiverRank{1}
		}
		return ReceiverRank{}
	})

	receivers := []*ReceiverCandidate{
		{Node: node("free-small", nil), AvailableResource: 1},
		{Node: node("stays-large", nil), StaysOccupied: true, AvailableResource: 6},
		{Node: node("stays-small", nil), StaysOccupied: true, AvailableResource: 2},
	}
	ordered := ssn.OrderReceivers(&PlanningCandidate{}, receivers)
	got := []string{ordered[0].Receiver.Node.Name, ordered[1].Receiver.Node.Name, ordered[2].Receiver.Node.Name}
	if fmt.Sprint(got) != "[stays-small stays-large free-small]" {
		t.Fatalf("receiver order=%v, want stability before best-fit", got)
	}
	if stabilityCalls != len(receivers) || fitCalls != len(receivers) {
		t.Fatalf("rank calls stability=%d fit=%d, want one call per receiver", stabilityCalls, fitCalls)
	}
}

func TestReceiverPoolChainsWithoutMutatingCallerSlice(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		nodes[0] = nil
		return nodes[1:]
	})
	original := []*schedapi.NodeInfo{node("a", nil), node("b", nil)}
	pool := ssn.ReceiverPool(original)
	if original[0] == nil || original[0].Name != "a" {
		t.Fatalf("caller slice was mutated: %v", original)
	}
	if len(pool) != 1 || pool[0].Name != "b" {
		t.Fatalf("pool=%v, want [b]", pool)
	}
}
