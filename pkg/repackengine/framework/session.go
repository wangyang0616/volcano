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
	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Callback contracts plugins register into a Session.
type (
	// MovableFn reports whether a task may be moved. Aggregated with AND — any
	// plugin may veto a move (gang breach, PDB, frozen scope).
	MovableFn func(task *schedapi.TaskInfo) bool
	// DomainFn enumerates the freeable units a domain contributes (node units,
	// hypernode units, ...). Aggregated by union; the core optimizes their combined
	// weighted benefit.
	DomainFn func(snapshot Snapshot) []api.FreeableUnit
	// DisruptionScoreFn scores a candidate plan on one dimension (higher = more
	// disruptive). Weighted + min-max normalized across candidates by the Session.
	// This is SOFT ranking (used by LeastDisruptive to pick among feasible plans).
	DisruptionScoreFn func(ctx *api.PlanContext, p *api.CandidatePlan) float64
	// PlanConstraintFn is a HARD admissibility gate on a finished plan: return
	// false to reject it outright. Aggregated with AND — any constraint may veto.
	// Distinct from DisruptionScoreFn (soft ranking): a failed constraint discards
	// the plan. The benefit gates (MinNodesFreed, MinFragImprovementPercent) are
	// registered as built-in constraints; later features like disruptionPolicy's
	// maxDisruptionScore add their own via AddConstraintFn.
	PlanConstraintFn func(ctx *api.PlanContext, plan *api.RepackPlan) bool
)

type scoreTerm struct {
	name   string
	weight float64
	fn     DisruptionScoreFn
}

// SessionConfig is the per-run input the driver supplies to OpenSession.
type SessionConfig struct {
	Snapshot      Snapshot
	Run           *repackv1alpha1.RepackRun
	Resource      v1.ResourceName
	Mode          repackv1alpha1.RepackMode
	CoreName      string      // selected search strategy (repack.core)
	Hooks         CommitHooks // Execute side effects (nil funcs for DryRun)
	MinNodesFreed int
	// MinFragImprovementPercent is the run's benefit gate from
	// spec.goals[0].minFragImprovementPercent (percentage points, 0-100): a plan
	// whose fragmentation improvement is below it is not worth it. 0 = no gate.
	MinFragImprovementPercent int
	MaxPodGroups              int
	MaxResource               int64
	LimitPodGroups            bool                                        // distinguishes omitted from explicit zero
	LimitResource             bool                                        // distinguishes omitted from explicit zero
	Free                      func(*schedapi.NodeInfo) *schedapi.Resource // nil = FutureIdle
}

// Session is one repack pass: a snapshot plus the callbacks plugins register,
// consumed by the core and actions. Mirrors framework.Session in the scheduler.
type Session struct {
	configuration SessionConfig
	plugins       []Plugin // opened plugins, for OnSessionClose

	movableFns    []MovableFn
	domainFns     []DomainFn
	scoreTerms    []scoreTerm
	constraintFns []PlanConstraintFn

	// results filled by the action, read by the driver
	plan         *api.RepackPlan
	report       Report
	commitResult *CommitResult // Execute-only; nil for DryRun or an empty plan
}

// OpenSession builds a Session and runs each named plugin's OnSessionOpen (which
// registers its callbacks). Unknown plugin names are ignored.
func OpenSession(configuration SessionConfig, pluginNames []string) *Session {
	ssn := &Session{configuration: configuration}
	ssn.registerBuiltinConstraints()
	for _, name := range pluginNames {
		p, ok := GetPlugin(name)
		if !ok {
			continue
		}
		p.OnSessionOpen(ssn)
		ssn.plugins = append(ssn.plugins, p)
	}
	return ssn
}

// registerBuiltinConstraints turns the run's benefit gates into first-class
// plan constraints, so the core just asks PlanAdmissible instead of hardcoding
// them. Additional plan-level policies (e.g. disruptionPolicy.maxDisruptionScore) join
// the same seam via AddConstraintFn.
func (s *Session) registerBuiltinConstraints() {
	// MinNodesFreed: a plan must free at least this many nodes (default 1).
	minFreed := s.configuration.MinNodesFreed
	if minFreed < 1 {
		minFreed = 1
	}
	s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
		return plan != nil && plan.Benefit() >= float64(minFreed)
	})
	// MinFragImprovementPercent: fragmentation must drop by at least this many
	// percentage points. FragmentationRateDelta is negative (fragmentation fell), so the
	// improvement is round(-delta*100). 0 = no gate.
	if minImprove := s.configuration.MinFragImprovementPercent; minImprove > 0 {
		s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
			if plan == nil {
				return false
			}
			improvePct := int(-plan.FragmentationRateDelta()*100 + 0.5)
			return improvePct >= minImprove
		})
	}
}

// CloseSession runs OnSessionClose on the plugins opened by OpenSession.
func CloseSession(ssn *Session) {
	for _, p := range ssn.plugins {
		p.OnSessionClose(ssn)
	}
	ssn.plugins = nil
}

// ---- registration (called by plugins in OnSessionOpen) ----

func (s *Session) AddMovableFn(fn MovableFn) {
	if fn != nil {
		s.movableFns = append(s.movableFns, fn)
	}
}
func (s *Session) AddDomainFn(fn DomainFn) {
	if fn != nil {
		s.domainFns = append(s.domainFns, fn)
	}
}
func (s *Session) AddDisruptionScoreFn(name string, weight float64, fn DisruptionScoreFn) {
	if fn != nil {
		s.scoreTerms = append(s.scoreTerms, scoreTerm{name: name, weight: weight, fn: fn})
	}
}
func (s *Session) AddConstraintFn(fn PlanConstraintFn) {
	if fn != nil {
		s.constraintFns = append(s.constraintFns, fn)
	}
}

// PlanAdmissible reports whether a finished plan passes every hard constraint
// (built-in benefit gates + any plugin-registered PlanConstraintFns), AND-
// aggregated: a single false rejects the plan. Soft ranking among admissible
// plans is LeastDisruptive; this is the hard veto the core applies before
// committing a plan.
func (s *Session) PlanAdmissible(plan *api.RepackPlan) bool {
	ctx := s.PlanContext()
	for _, fn := range s.constraintFns {
		if !fn(ctx, plan) {
			return false
		}
	}
	return true
}

// ---- config accessors ----

func (s *Session) Snapshot() Snapshot              { return s.configuration.Snapshot }
func (s *Session) Run() *repackv1alpha1.RepackRun  { return s.configuration.Run }
func (s *Session) Resource() v1.ResourceName       { return s.configuration.Resource }
func (s *Session) Mode() repackv1alpha1.RepackMode { return s.configuration.Mode }
func (s *Session) CoreName() string                { return s.configuration.CoreName }
func (s *Session) Hooks() CommitHooks              { return s.configuration.Hooks }
func (s *Session) MinNodesFreed() int              { return s.configuration.MinNodesFreed }
func (s *Session) MinFragImprovementPercent() int  { return s.configuration.MinFragImprovementPercent }
func (s *Session) MaxPodGroups() int               { return s.configuration.MaxPodGroups }
func (s *Session) MaxResource() int64              { return s.configuration.MaxResource }
func (s *Session) LimitPodGroups() bool            { return s.configuration.LimitPodGroups }
func (s *Session) LimitResource() bool             { return s.configuration.LimitResource }

// Free returns the node free-capacity basis (default NodeInfo.FutureIdle).
func (s *Session) Free() func(*schedapi.NodeInfo) *schedapi.Resource {
	if s.configuration.Free != nil {
		return s.configuration.Free
	}
	return func(n *schedapi.NodeInfo) *schedapi.Resource { return n.FutureIdle() }
}

// ---- aggregate consumption (called by the core/actions) ----

// Nodes returns the snapshot's candidate nodes.
func (s *Session) Nodes() []*schedapi.NodeInfo { return s.configuration.Snapshot.Nodes() }

// FeasibleRelocation delegates to the snapshot's scheduler-faithful relocation
// feasibility check: simulate evicting victims and greedily place them onto receivers with the
// full scheduler filter stack. See Snapshot.FeasibleRelocation.
func (s *Session) FeasibleRelocation(committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return s.configuration.Snapshot.FeasibleRelocation(committed, victims, receivers)
}

// Movable returns an api.Movable that is the AND of all registered MovableFns
// (no plugins → everything movable).
func (s *Session) Movable() api.Movable {
	fns := s.movableFns
	return func(t *schedapi.TaskInfo) bool {
		if t == nil {
			return false
		}
		for _, fn := range fns {
			if !fn(t) {
				return false
			}
		}
		return true
	}
}

// FreeableUnits is the union of every domain plugin's units. With both node and
// hypernode domains enabled this carries both levels, and Benefit/LeastDisruptive
// let the core weigh them jointly (holistic optimum).
func (s *Session) FreeableUnits() []api.FreeableUnit {
	var out []api.FreeableUnit
	for _, fn := range s.domainFns {
		out = append(out, fn(s.configuration.Snapshot)...)
	}
	return out
}

// PlanContext builds the scoring context from the snapshot and target resource.
func (s *Session) PlanContext() *api.PlanContext {
	return &api.PlanContext{TargetResource: s.configuration.Resource, PodGroupViews: s.configuration.Snapshot}
}

// CurrentFragmentationRate is the target resource's fragmentation rate over the snapshot's
// nodes, independent of any plan. Used to fill report.FragmentationRateBefore when the core
// returns no plan, so the driver can tell a clean cluster (NoFragmentation) apart
// from a fragmented one with no worthwhile plan (BelowGoalThreshold).
func (s *Session) CurrentFragmentationRate() float64 {
	return api.MeasureResourceFragmentation(s.Nodes(), s.configuration.Resource).FragmentationRate()
}

// LeastDisruptive returns the index of the least-disruptive candidate, applying
// the registered score terms with min-max normalization across the batch (a term
// where all candidates tie contributes nothing). Ties keep the earliest index, so
// callers should pass candidates in a meaningful order (e.g. max benefit first).
// Returns 0 for a single/empty batch.
func (s *Session) LeastDisruptive(cands []*api.CandidatePlan) int {
	if len(cands) <= 1 {
		return 0
	}
	ctx := s.PlanContext()
	totals := make([]float64, len(cands))
	for _, t := range s.scoreTerms {
		if t.weight <= 0 {
			continue
		}
		raw := make([]float64, len(cands))
		mn, mx := 0.0, 0.0
		for i, p := range cands {
			raw[i] = t.fn(ctx, p)
			if i == 0 || raw[i] < mn {
				mn = raw[i]
			}
			if i == 0 || raw[i] > mx {
				mx = raw[i]
			}
		}
		span := mx - mn
		for i := range cands {
			norm := 0.0
			if span > 0 {
				norm = (raw[i] - mn) / span
			}
			totals[i] += t.weight * norm
		}
	}
	best, bestScore := 0, totals[0]
	for i, sc := range totals {
		if sc < bestScore {
			best, bestScore = i, sc
		}
	}
	return best
}

// ---- result (set by the action, read by the driver) ----

func (s *Session) SetPlan(p *api.RepackPlan)            { s.plan = p }
func (s *Session) Plan() *api.RepackPlan                { return s.plan }
func (s *Session) SetReport(r Report)                   { s.report = r }
func (s *Session) Report() Report                       { return s.report }
func (s *Session) SetCommit(commitResult *CommitResult) { s.commitResult = commitResult }
func (s *Session) Commit() *CommitResult                { return s.commitResult }
