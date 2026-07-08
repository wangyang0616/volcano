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
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// process plans and acts on a cleared run (the gate already passed).
func (e *Engine) process(work *repackv1alpha1.RepackRun) {
	start := time.Now()
	defer func() { metrics.ObserveCycle(string(work.Spec.Mode), time.Since(start).Seconds()) }()
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		// Defers run LIFO: markExecuteDone (declared last) releases the K=1 slot
		// first, then requeueGatedRuns (declared first) re-enqueues Execute runs
		// that were blocked on it. Without the wake, a run gated with reason
		// AnotherRunActive — which carries no RequeueAfter — would never be
		// revisited under the event-driven (resync=0) model and would be starved.
		defer e.requeueGatedRuns()
		// Release the K=1 slot and stamp the cooldown anchor when done, even on
		// panic/early-return paths.
		defer e.markExecuteDone(work.Name)
	}
	e.mu.Lock()
	tiers, cfgs := e.tiers, e.configurations
	e.mu.Unlock()

	sched := schedframework.OpenSession(e.cache, tiers, cfgs)
	// Read-only close: the engine plans against the session but must NOT write back
	// PodGroup/Queue status (gang Unschedulable conditions, JobUpdater, queue
	// allocated) — that is the scheduler's job and a second writer would race it.
	defer schedframework.CloseSessionReadOnly(sched)

	gen := work.Generation
	res := e.resolveResource(work)
	if res == "" {
		// No target accelerator resolvable: spec.goals is empty AND the engine's
		// --repack-default-resource is unset. Measuring fragmentation on the empty
		// resource would count every node as empty and silently report
		// NoFragmentation, so fail fast with an actionable reason instead.
		e.fail(work, gen, "NoTargetResource",
			fmt.Errorf("no target accelerator resource: set spec.goals[0].resource or the engine flag --repack-default-resource"))
		return
	}
	if !supportedTarget(res) {
		// Only fully-qualified extended resources (nvidia.com/gpu, huawei.com/Ascend910)
		// are supported — they live in Resource.ScalarResources. Core compute resources
		// (cpu, memory, ephemeral-storage, ...) are stored in dedicated fields, so
		// Scalar() reads 0 for them and the run would be a silent no-op reporting
		// NoFragmentation. CEL rejects this on spec.goals, but the --repack-default-
		// resource fallback bypasses CEL, so guard it here too.
		e.fail(work, gen, "UnsupportedResource",
			fmt.Errorf("target resource %q is not supported; only fully-qualified extended resources (e.g. nvidia.com/gpu) can be defragmented, not core resources like cpu/memory", res))
		return
	}

	reason := state.ReasonSimulating
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		reason = state.ReasonEvicting
	}
	state.SetCondition(&work.Status.Conditions, state.CondQueued, metav1.ConditionFalse, state.ReasonSlotAcquired, "slot acquired", gen)
	state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionTrue, reason, "engine started", gen)
	work.Status.Phase = state.DerivePhase(work.Status.Conditions)
	e.updateStatus(work)

	inScope, nodeInScope, err := engineframework.ResolveScope(work.Spec.Scope, adapter.SessionGangInfo(sched))
	if err != nil {
		e.fail(work, gen, "ScopeError", err)
		return
	}

	snap := adapter.NewSessionSnapshot(sched, res, nodeInScope)
	maxPG, maxRes := maxPerRun(work, res)
	esn := engineframework.OpenSession(engineframework.SessionConfig{
		Snapshot:                  snap,
		Run:                       work,
		Resource:                  res,
		Mode:                      work.Spec.Mode,
		CoreName:                  e.cfg.Core,
		MinNodesFreed:             e.cfg.MinNodesFreed,
		MinFragImprovementPercent: minFragImprovement(work),
		MaxPodGroups:              maxPG,
		MaxResource:               maxRes,
		Hooks:                     hooksFor(work.Spec.Mode, e.cache.Client()),
	}, e.cfg.Plugins)
	esn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return inScope(t.Job) })
	defer engineframework.CloseSession(esn)

	// The repack action runs the core and (Execute) evicts via Hooks; open-loop —
	// a failed eviction is recorded, not fatal.
	engineframework.RunActions(e.cfg.Actions, esn)

	report, plan := esn.Report(), esn.Plan()
	klog.V(3).InfoS("plan computed", "run", work.Name, "mode", work.Spec.Mode, "resource", res,
		"freedNodes", report.NodesFreed, "movedCards", report.MovedResource,
		"affectedPodGroups", report.AffectedPodGroups,
		"fragBeforePct", pct(report.FragRateBefore), "fragAfterPct", pct(report.FragRateAfter))
	// The Complete reason doubles as the "worth repacking?" verdict (§5.2.2);
	// there is no summary.verdict. worthwhile = the plan freed nodes; an empty
	// plan splits by whether fragmentation existed: none (clean) vs below the
	// benefit gate (fragmented but not worth acting on).
	worthwhile := report.NodesFreed > 0
	execute := work.Spec.Mode == repackv1alpha1.RepackModeExecute
	ttl := time.Duration(0)
	if execute {
		ttl = e.cfg.NominationTTL
	}
	applyPlan(work, report, plan, res, execute, ttl)
	if commit := esn.Commit(); commit != nil {
		metrics.ObserveEvictions(len(commit.Evicted), len(commit.Failed))
		klog.V(3).InfoS("evictions issued", "run", work.Name,
			"evicted", len(commit.Evicted), "rejected", len(commit.Failed))
	}

	// Execute with a worthwhile plan: if every eviction was rejected (e.g. by PDBs)
	// the repack achieved nothing — fail rather than falsely reporting Executed.
	if execute && worthwhile {
		if commit := esn.Commit(); commit != nil && len(commit.Evicted) == 0 && len(commit.Failed) > 0 {
			e.fail(work, gen, state.ReasonExecuteFailed,
				fmt.Errorf("all %d evictions were rejected; no pods were moved", len(commit.Failed)))
			return
		}
	}

	var done, msg string
	switch {
	case !worthwhile && report.FragRateBefore > 0:
		done, msg = state.ReasonBelowGoalThreshold, "engine finished"
	case !worthwhile:
		done, msg = state.ReasonNoFragmentation, "engine finished"
	case execute:
		done, msg = state.ReasonExecuted, "engine finished"
		// Partial success: some evictions were rejected but at least one succeeded.
		if commit := esn.Commit(); commit != nil && len(commit.Failed) > 0 {
			msg = fmt.Sprintf("evicted %d pods; %d evictions were rejected", len(commit.Evicted), len(commit.Failed))
		}
	default:
		done, msg = state.ReasonRepackRecommended, "engine finished"
	}
	state.SetCondition(&work.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, done, msg, gen)
	state.SetCondition(&work.Status.Conditions, state.CondComplete, metav1.ConditionTrue, done, msg, gen)
	work.Status.Phase = state.DerivePhase(work.Status.Conditions)
	klog.V(3).InfoS("RepackRun finished", "run", work.Name, "mode", work.Spec.Mode, "outcome", done)
	e.updateStatusTerminal(work)
}

// hooksFor returns the commit side effects. DryRun: none. Execute: evict each
// victim via the Eviction API (PDB-respecting; the workload controller then
// recreates the pod, steered by the nomination reconciler). No reservation/taint.
func hooksFor(mode repackv1alpha1.RepackMode, kube kubernetes.Interface) engineframework.CommitHooks {
	if mode != repackv1alpha1.RepackModeExecute {
		return engineframework.CommitHooks{}
	}
	return engineframework.CommitHooks{
		Evict: func(m *engineapi.Move) error {
			if m == nil || m.Task == nil || m.Task.Pod == nil {
				return nil
			}
			pod := m.Task.Pod
			return kube.PolicyV1().Evictions(pod.Namespace).Evict(context.Background(), &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			})
		},
	}
}

func (e *Engine) resolveResource(run *repackv1alpha1.RepackRun) v1.ResourceName {
	if len(run.Spec.Goals) > 0 && run.Spec.Goals[0].Resource != "" {
		return run.Spec.Goals[0].Resource
	}
	return v1.ResourceName(e.cfg.DefaultResource)
}

// supportedTarget reports whether res is a defragmentable accelerator resource.
// Extended resources are fully qualified with a domain prefix (contain "/"), e.g.
// nvidia.com/gpu; core resources (cpu, memory, ephemeral-storage, pods,
// hugepages-*) are not and are unsupported. This mirrors the CEL rule on
// spec.goals[0].resource and also guards the --repack-default-resource fallback.
func supportedTarget(res v1.ResourceName) bool {
	return strings.Contains(string(res), "/")
}
