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
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

// fakeSnap is a minimal framework.Snapshot for solver tests (every node fits).
type fakeSnap struct {
	nodes      []*schedapi.NodeInfo
	views      map[schedapi.JobID]api.PodGroupView
	notInScope map[string]bool // node name → excluded from draining (receiver only)
}

func (f *fakeSnap) Nodes() []*schedapi.NodeInfo { return f.nodes }
func (f *fakeSnap) NodeInScope(n *schedapi.NodeInfo) bool {
	if f.notInScope == nil {
		return true
	}
	return !f.notInScope[n.Name]
}
func (f *fakeSnap) PodGroupView(id schedapi.JobID) api.PodGroupView {
	return f.views[id]
}
func (f *fakeSnap) Predicate(*schedapi.TaskInfo, *schedapi.NodeInfo) error { return nil }

func gpuRes(n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

func gpuTask(name, gang string, g int64) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, Job: schedapi.JobID(gang), InitResreq: gpuRes(g)}
}

// sysTask is a non-accelerator pod (e.g. a system DaemonSet): CPU-only request,
// no gang. It must not count toward node-freeability of the accelerator.
func sysTask(name string) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, InitResreq: &schedapi.Resource{MilliCPU: 100}}
}

func capNode(name string, capGPU int64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	m := map[schedapi.TaskID]*schedapi.TaskInfo{}
	var used int64
	for i, t := range tasks {
		t.NodeName = name
		m[schedapi.TaskID(fmt.Sprintf("%s-%d", name, i))] = t
		used += int64(t.InitResreq.ScalarResources[gpu] + 0.5)
	}
	return &schedapi.NodeInfo{Name: name, Tasks: m, Allocatable: gpuRes(capGPU), Used: gpuRes(used)}
}

// freeByCapMinusUsed gives each node free GPU = Allocatable − Used, so tests need
// not wrangle NodeInfo.Idle/FutureIdle.
func freeByCapMinusUsed(n *schedapi.NodeInfo) *schedapi.Resource {
	return gpuRes(int64(n.Allocatable.ScalarResources[gpu] - n.Used.ScalarResources[gpu]))
}

// nodeUnits is the node-domain contribution (one single-node unit per in-scope
// node — mirrors the real node plugin, which gates drain targets by NodeInScope).
func nodeUnits(snap framework.Snapshot) []api.FreeableUnit {
	out := make([]api.FreeableUnit, 0, len(snap.Nodes()))
	for _, n := range snap.Nodes() {
		if !snap.NodeInScope(n) {
			continue
		}
		out = append(out, api.FreeableUnit{Level: "node", Nodes: []string{n.Name}, Weight: 1})
	}
	return out
}

func drainSession(snap framework.Snapshot, movable framework.MovableFn, minFreed int, maxPG int, maxRes int64) *framework.Session {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:      snap,
		Resource:      gpu,
		Mode:          repackv1alpha1.RepackModeDryRun,
		CoreName:      framework.CoreDrain,
		MinNodesFreed: minFreed,
		MaxPodGroups:  maxPG,
		MaxResource:   maxRes,
		Free:          freeByCapMinusUsed,
	}, nil)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(movable)
	return ssn
}

func allMovable(*schedapi.TaskInfo) bool { return true }

func realMoves(plan *api.RepackPlan) []*api.Move {
	var out []*api.Move
	for _, m := range plan.Moves {
		if m != nil && m.To != m.From {
			out = append(out, m)
		}
	}
	return out
}

// n0 holds a small gang (2 GPU) that fits into n1's slack; draining n0 frees a
// whole node with a single move. n1 (6 GPU used) cannot be freed.
func TestDrain_FreesOneNode(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	plan, ok := (&drainCore{}).Plan(drainSession(snap, allMovable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n0" {
		t.Fatalf("freed=%v, want [n0]", plan.FreedNodes)
	}
	mv := realMoves(plan)
	if len(mv) != 1 || mv[0].Task.Name != "a" || mv[0].From != "n0" || mv[0].To != "n1" {
		t.Fatalf("moves=%+v, want a:n0->n1", mv)
	}
	if plan.Benefit() != 1 {
		t.Errorf("benefit=%v, want 1", plan.Benefit())
	}
}

// MinNodesFreed=2 is unreachable here (only one node can be vacated) → NoRepack.
func TestDrain_RejectsBelowMinFreed(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	if plan, ok := (&drainCore{}).Plan(drainSession(snap, allMovable, 2, 0, 0)); ok {
		t.Fatalf("expected NoRepack (min 2 nodes), got %+v", plan)
	}
}

// A maxResource budget below the victim's footprint blocks the only move.
func TestDrain_BudgetBlocks(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	if _, ok := (&drainCore{}).Plan(drainSession(snap, allMovable, 1, 0, 1)); ok {
		t.Fatal("expected NoRepack: maxResource=1 < victim 2")
	}
}

// A frozen task makes its node un-vacatable as a drain TARGET, but the node is
// still a valid RECEIVER: here g-a (on n0) is frozen, so drain frees n1 instead
// by moving b onto n0's slack.
func TestDrain_FrozenNodeSkippedAsTarget(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	frozen := func(t *schedapi.TaskInfo) bool { return t.Job != "g-a" } // g-a frozen
	plan, ok := (&drainCore{}).Plan(drainSession(snap, frozen, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected n1 to be freed (n0 frozen but usable as receiver)")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n1" {
		t.Fatalf("freed=%v, want [n1]", plan.FreedNodes)
	}
	mv := realMoves(plan)
	if len(mv) != 1 || mv[0].Task.Name != "b" || mv[0].From != "n1" || mv[0].To != "n0" {
		t.Fatalf("moves=%+v, want b:n1->n0", mv)
	}
}

// Nothing movable anywhere → no node can be vacated → NoRepack.
func TestDrain_AllFrozenNoRepack(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	none := func(*schedapi.TaskInfo) bool { return false }
	if _, ok := (&drainCore{}).Plan(drainSession(snap, none, 1, 0, 0)); ok {
		t.Fatal("expected NoRepack: nothing movable")
	}
}

// The dynamic single pass must be deterministic: identical input → identical
// plan (guards against map-iteration-order leaking into the result).
func TestDrain_Deterministic(t *testing.T) {
	mk := func() *fakeSnap {
		return &fakeSnap{nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 2)),
			capNode("n2", 8, gpuTask("c", "g-c", 6)),
		}}
	}
	p1, ok1 := (&drainCore{}).Plan(drainSession(mk(), allMovable, 1, 0, 0))
	p2, ok2 := (&drainCore{}).Plan(drainSession(mk(), allMovable, 1, 0, 0))
	if !ok1 || !ok2 {
		t.Fatalf("expected feasible plans (ok1=%v ok2=%v)", ok1, ok2)
	}
	if s1, s2 := planKey(p1), planKey(p2); s1 != s2 {
		t.Fatalf("nondeterministic plan:\n  %s\n  %s", s1, s2)
	}
}

// planKey is a stable string fingerprint of a plan (freed nodes + moves), for
// deterministic comparison across runs.
func planKey(p *api.RepackPlan) string {
	freed := append([]string(nil), p.FreedNodes...)
	sort.Strings(freed)
	mv := make([]string, 0, len(p.Moves))
	for _, m := range p.Moves {
		if m != nil && m.To != m.From {
			mv = append(mv, fmt.Sprintf("%s:%s->%s", m.Task.Name, m.From, m.To))
		}
	}
	sort.Strings(mv)
	return fmt.Sprintf("freed=%v moves=%v", freed, mv)
}

// A staying node (holds a frozen pod → can never be vacated) should be the
// preferred receiver, so filling it does not waste a drainable node. Here n2
// (frozen f) and n3 (movable m) both have free 6, but the node list puts the
// drainable n3 first — plain best-fit (a tie) would fill n3 and lose it, freeing
// only 1 node; the staying-receiver preference sends the victim to n2, keeping n3
// drainable → 2 nodes freed.
func TestDrain_PrefersStayingReceiver(t *testing.T) {
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("n0", 8, gpuTask("a", "g-a", 2)),
		capNode("n3", 8, gpuTask("m", "g-m", 2)),
		capNode("n2", 8, gpuTask("f", "g-f", 2)),
	}}
	movable := func(tk *schedapi.TaskInfo) bool { return tk.Job != "g-f" } // g-f frozen
	plan, ok := (&drainCore{}).Plan(drainSession(snap, movable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	if len(plan.FreedNodes) != 2 {
		t.Fatalf("freed=%v, want 2 (n0,n3 kept drainable via staying-receiver preference)", plan.FreedNodes)
	}
	// The frozen node n2 must never be a drain target.
	for _, n := range plan.FreedNodes {
		if n == "n2" {
			t.Errorf("n2 (frozen) must not be vacated; freed=%v", plan.FreedNodes)
		}
	}
}

// scope.nodes.exclude = "don't drain but may receive": n2 is excluded from
// draining, so it is never a target but is the preferred receiver — a,b land on
// it and the two in-scope nodes free.
func TestDrain_ExcludedNodeIsReceiverNotTarget(t *testing.T) {
	snap := &fakeSnap{
		nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 2)),
			capNode("n2", 8, gpuTask("c", "g-c", 2)),
		},
		notInScope: map[string]bool{"n2": true}, // excluded from draining
	}
	plan, ok := (&drainCore{}).Plan(drainSession(snap, allMovable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	for _, n := range plan.FreedNodes {
		if n == "n2" {
			t.Fatalf("n2 (excluded from draining) must not be a target; freed=%v", plan.FreedNodes)
		}
	}
	if len(plan.FreedNodes) != 2 {
		t.Fatalf("freed=%v, want 2 (n0,n1; n2 absorbs as preferred receiver)", plan.FreedNodes)
	}
}

// A node running a non-movable, non-accelerator pod (a system DaemonSet) is still
// freeable: only its accelerator pods must be movable, and only they are evicted.
// Regression for the bug where any pinned system pod made every real accelerator
// node unfreeable (repack no-op in production).
func TestDrain_SystemPodDoesNotBlockFreeing(t *testing.T) {
	a := gpuTask("a", "g-a", 2)  // movable GPU pod on n0
	sys := sysTask("kube-proxy") // pinned, non-accelerator, out-of-scope pod on n0
	b := gpuTask("b", "g-b", 6)  // n1 stays occupied
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a, sys), capNode("n1", 8, b)}}

	// Everything movable except the system pod (no gang → out of scope in reality).
	movable := func(t *schedapi.TaskInfo) bool { return t.Name != "kube-proxy" }

	plan, ok := (&drainCore{}).Plan(drainSession(snap, movable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan: n0's GPU pod can move; the system pod stays")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n0" {
		t.Fatalf("freed=%v, want [n0]", plan.FreedNodes)
	}
	if mv := realMoves(plan); len(mv) != 1 || mv[0].Task.Name != "a" {
		t.Fatalf("moves=%+v, want only the GPU pod a (system pod must not move)", mv)
	}
}

// drainSessionFragGate mirrors drainSession but sets the per-run frag-improvement
// gate (spec.goals[0].minFragImprovementPercent).
func drainSessionFragGate(snap framework.Snapshot, minImprovePct int) *framework.Session {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:                  snap,
		Resource:                  gpu,
		Mode:                      repackv1alpha1.RepackModeDryRun,
		CoreName:                  framework.CoreDrain,
		MinNodesFreed:             1,
		MinFragImprovementPercent: minImprovePct,
		Free:                      freeByCapMinusUsed,
	}, nil)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)
	return ssn
}

// Freeing n0 of two GPU nodes cuts fragmentation by 1/2 = 50 percentage points.
// A gate at 50 admits the plan (improvement meets the bar); a gate at 60 rejects
// it even though a node could be freed (below the run's benefit threshold).
func TestDrain_FragImprovementGate(t *testing.T) {
	newSnap := func() *fakeSnap {
		return &fakeSnap{nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 6)),
		}}
	}
	if plan, ok := (&drainCore{}).Plan(drainSessionFragGate(newSnap(), 50)); !ok || plan == nil {
		t.Fatalf("gate=50: expected a feasible plan (50pp improvement meets the bar)")
	}
	if plan, ok := (&drainCore{}).Plan(drainSessionFragGate(newSnap(), 60)); ok {
		t.Fatalf("gate=60: expected NoRepack (50pp improvement below the bar), got %+v", plan)
	}
}

const e2eNPU = v1.ResourceName("volcano.sh/e2e-npu")

// TestDrain_E2EMilliNPULayout mirrors the e2e two-halves-into-one scenario at the
// scheduler's milli scale (8→8000, 4→4000, 2→2000). Ledger math must work even
// when Scalar() returns thousands, not single-digit card counts.
func TestDrain_E2EMilliNPULayout(t *testing.T) {
	npuRes := func(n float64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{e2eNPU: n}}
	}
	npuTask := func(name, gang string, cards float64) *schedapi.TaskInfo {
		return &schedapi.TaskInfo{Name: name, Job: schedapi.JobID(gang), InitResreq: npuRes(cards)}
	}
	capNPUNode := func(name string, cap, used float64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
		m := map[schedapi.TaskID]*schedapi.TaskInfo{}
		for i, t := range tasks {
			t.NodeName = name
			m[schedapi.TaskID(fmt.Sprintf("%s-%d", name, i))] = t
		}
		return &schedapi.NodeInfo{
			Name: name, Tasks: m,
			Allocatable: npuRes(cap),
			Used:        npuRes(used),
		}
	}
	freeNPU := func(n *schedapi.NodeInfo) *schedapi.Resource {
		return npuRes(n.Allocatable.ScalarResources[e2eNPU] - n.Used.ScalarResources[e2eNPU])
	}

	a := npuTask("a", "g-a", 4000)
	b := npuTask("b", "g-b", 2000)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNPUNode("n0", 8000, 4000, a),
		capNPUNode("n1", 8000, 2000, b),
		capNPUNode("n2", 8000, 0),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snap, Resource: e2eNPU, CoreName: framework.CoreDrain,
		MinNodesFreed: 1, Free: freeNPU,
	}, nil)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)

	plan, ok := (&drainCore{}).Plan(ssn)
	if !ok || plan == nil {
		t.Fatal("expected consolidation plan for milli-scale e2e layout")
	}
	if len(plan.FreedNodes) != 1 {
		t.Fatalf("freed=%v, want one node", plan.FreedNodes)
	}
	if mv := realMoves(plan); len(mv) != 1 || mv[0].To == mv[0].From {
		t.Fatalf("moves=%+v, want one real relocation", mv)
	}
}
