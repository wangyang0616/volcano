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
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Movable is the AND of every registered MovableFn (any plugin may veto).
func TestSession_MovableAND(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return t.Name != "x" })
	ssn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return t.Job != "frozen" })
	mv := ssn.Movable()
	if !mv(task("a", "g", 1)) {
		t.Error("a should be movable")
	}
	if mv(task("x", "g", 1)) {
		t.Error("x vetoed by first fn")
	}
	if mv(task("a", "frozen", 1)) {
		t.Error("frozen vetoed by second fn")
	}
}

// No MovableFn registered → everything movable.
func TestSession_MovableEmptyAllMovable(t *testing.T) {
	if !newSession(&fakeSnap{}).Movable()(task("a", "g", 1)) {
		t.Error("no fns → all movable")
	}
}

// Node feasibility (taints/affinity/topology/resources) now lives in the scheduler-
// faithful Snapshot.FeasibleRelocation feasibility check (adapter), exercised by the drain and
// e2e suites — the session no longer has a Predicate path to unit-test here.

// FreeableUnits is the union across domain plugins (node + hypernode here).
func TestSession_FreeableUnitsUnion(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddDomainFn(func(Snapshot) []api.FreeableUnit {
		return []api.FreeableUnit{{Level: "node", Nodes: []string{"n0"}, Weight: 1}}
	})
	ssn.AddDomainFn(func(Snapshot) []api.FreeableUnit {
		return []api.FreeableUnit{{Level: "hypernode", Nodes: []string{"n0", "n1"}, Weight: 3}}
	})
	if u := ssn.FreeableUnits(); len(u) != 2 {
		t.Fatalf("union len=%d, want 2", len(u))
	}
}

func TestSession_DisruptionScoresExplainNormalizationAndWeighting(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddDisruptionScoreFn("movedPods", 1, func(ctx *api.PlanContext, plan *api.CandidatePlan) int64 {
		return plan.MoveAggregate(ctx).MovedPods
	})
	ssn.AddDisruptionScoreFn("constantRisk", 2, func(*api.PlanContext, *api.CandidatePlan) int64 {
		return 7
	})
	cheap := &api.CandidatePlan{Moves: []*api.Move{
		move(task("a", "ga", 1), "n0", "n1"),
	}}
	costly := &api.CandidatePlan{Moves: []*api.Move{
		move(task("b", "gb", 1), "n0", "n1"),
		move(task("c", "gc", 1), "n0", "n1"),
	}}

	scores := ssn.DisruptionScores([]*api.CandidatePlan{costly, cheap})
	if len(scores) != 2 || len(scores[0].Terms) != 2 || len(scores[1].Terms) != 2 {
		t.Fatalf("scores=%+v, want two candidates with two term explanations each", scores)
	}
	if scores[0].Total != 200 || scores[1].Total != 300 {
		t.Fatalf("totals=(%v,%v), want (200,300)", scores[0].Total, scores[1].Total)
	}
	movedPods := scores[0].Terms[0]
	if movedPods.Name != "movedPods" || movedPods.Raw != 2 || movedPods.Weight != 1 ||
		movedPods.Score != 0 || movedPods.Contribution != 0 {
		t.Errorf("costly movedPods explanation=%+v", movedPods)
	}
	constantRisk := scores[0].Terms[1]
	if constantRisk.Name != "constantRisk" || constantRisk.Raw != 7 ||
		constantRisk.Score != 100 || constantRisk.Contribution != 200 {
		t.Errorf("constant term explanation=%+v; tied terms must receive the same maximum score", constantRisk)
	}
	if cheapMovedPods := scores[1].Terms[0]; cheapMovedPods.Score != 100 || cheapMovedPods.Contribution != 100 {
		t.Errorf("cheap movedPods explanation=%+v, want score=100 contribution=100", cheapMovedPods)
	}
}

func TestSession_DisruptionScoresSkipZeroWeight(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	calls := 0
	ssn.AddDisruptionScoreFn("disabled", 0, func(*api.PlanContext, *api.CandidatePlan) int64 {
		calls++
		return 100
	})

	scores := ssn.DisruptionScores([]*api.CandidatePlan{{}, {}})
	if calls != 0 {
		t.Fatalf("disabled score function called %d times, want 0", calls)
	}
	for index, score := range scores {
		if score.Total != 0 || len(score.Terms) != 0 {
			t.Errorf("score[%d]=%+v, want no contribution or explanation for disabled term", index, score)
		}
	}
}

func TestSession_DisruptionScoresUseIntegerRange(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddDisruptionScoreFn("cost", 3, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		return int64(len(plan.Moves))
	})

	candidates := []*api.CandidatePlan{
		{Moves: []*api.Move{{}}},
		{Moves: []*api.Move{{}, {}}},
		{Moves: []*api.Move{{}, {}, {}, {}}},
	}
	scores := ssn.DisruptionScores(candidates)
	wantStrategyScores := []int64{100, 67, 0}
	for index, want := range wantStrategyScores {
		if got := scores[index].Terms[0].Score; got != want {
			t.Errorf("candidate[%d] strategy score=%d, want %d", index, got, want)
		}
		if got := scores[index].Total; got != want*3 {
			t.Errorf("candidate[%d] total=%d, want %d", index, got, want*3)
		}
	}
}
