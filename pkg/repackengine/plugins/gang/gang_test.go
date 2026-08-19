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

package gang

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

func gpuRes(n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

func tk(name, gang string, g int64) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, Job: schedapi.JobID(gang), InitResreq: gpuRes(g)}
}

func mv(t *schedapi.TaskInfo) *api.Move { return &api.Move{Task: t, From: "n0", To: "n1"} }

// fixedView is a test PodGroupViewer returning the same gang facts for every id.
type fixedView struct{ view api.PodGroupView }

func (f fixedView) PodGroupView(schedapi.JobID) api.PodGroupView { return f.view }

// gang g: Running=4, MinAvailable=3 → slack=1, Footprint=8.
func gangCtx() *api.PlanContext {
	return &api.PlanContext{
		TargetResource: gpu,
		PodGroupViews:  fixedView{view: api.PodGroupView{MinAvailable: 3, Running: 4, Footprint: 8}},
	}
}

// ScoreDamagedResource is a step function: within slack only the moved cards count;
// once minAvailable is breached the whole gang Footprint counts.
func TestScoreDamagedGPU_StepFunction(t *testing.T) {
	ctx := gangCtx()

	within := &api.CandidatePlan{Moves: []*api.Move{mv(tk("p0", "ns/g", 2))}} // 1 pod ≤ slack 1
	if s := scoreDamagedResource(ctx, within); s != 2 {
		t.Errorf("within slack: damaged=%v, want 2 (moved cards)", s)
	}

	breach := &api.CandidatePlan{Moves: []*api.Move{
		mv(tk("p0", "ns/g", 2)), mv(tk("p1", "ns/g", 2)), // 2 pods > slack 1
	}}
	if s := scoreDamagedResource(ctx, breach); s != 8 {
		t.Errorf("breach: damaged=%v, want 8 (whole footprint)", s)
	}
}

// ScoreGangBreaches counts gangs pushed below minAvailable.
func TestScoreGangBreaches(t *testing.T) {
	ctx := gangCtx()

	noBreach := &api.CandidatePlan{Moves: []*api.Move{mv(tk("p0", "ns/g", 1))}} // 1 ≤ slack 1
	if s := scoreGangBreaches(ctx, noBreach); s != 0 {
		t.Errorf("no breach: %v, want 0", s)
	}

	breach := &api.CandidatePlan{Moves: []*api.Move{
		mv(tk("p0", "ns/g", 1)), mv(tk("p1", "ns/g", 1)), // 2 > slack 1
	}}
	if s := scoreGangBreaches(ctx, breach); s != 1 {
		t.Errorf("breach: %v, want 1", s)
	}
}

func TestScoreFutureReceiverImpactUsesMarginalGangCost(t *testing.T) {
	ctx := gangCtx()
	candidate := &framework.PlanningCandidate{Plan: &api.CandidatePlan{
		Moves: []*api.Move{mv(tk("p0", "ns/g", 2))}, // consumes the gang's one-pod slack
	}}
	receiver := &framework.ReceiverCandidate{FutureMoves: map[schedapi.JobID]api.PodGroupMoveAggregate{
		"ns/g": {MovedPods: 1, MovedResource: 2},
	}}

	// Filling this receiver prevents a future drain that would newly breach the
	// gang. Damaged resource grows from the two moved cards to the full footprint.
	want := framework.ReceiverRank{1, 0, 6, 2, 1}
	if got := scoreFutureReceiverImpact(ctx, candidate, receiver); !reflect.DeepEqual(got, want) {
		t.Fatalf("future receiver rank=%v, want %v", got, want)
	}
}
