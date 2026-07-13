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

// process plans and acts on a cleared run (the gate already passed). Execute is
// deliberately two-phase: persist the complete plan and nomination intents
// first, then issue evictions. This closes the replacement-pod race and ensures
// a crash never performs an eviction whose intent was not durably recorded.
func (e *Engine) process(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	processingStartTime := time.Now()
	defer func() { metrics.ObserveCycle(string(run.Spec.Mode), time.Since(processingStartTime).Seconds()) }()
	if run.Spec.Mode == repackv1alpha1.RepackModeExecute {
		// Defers run LIFO: markExecuteDone (declared last) releases the K=1 slot
		// first, then requeueGatedRuns (declared first) re-enqueues Execute runs
		// that were blocked on it. Without the wake, a run gated with reason
		// AnotherRunActive — which carries no RequeueAfter — would never be
		// revisited under the event-driven (resync=0) model and would be starved.
		defer e.requeueGatedRuns()
		// Release the K=1 slot and stamp the cooldown anchor when done, even on
		// panic/early-return paths.
		defer e.markExecuteDone(run.Name)
	}
	e.engineStateMutex.Lock()
	schedulerTiers, schedulerConfigurations := e.tiers, e.configurations
	e.engineStateMutex.Unlock()

	schedulerSession := schedframework.OpenSession(e.schedulerCache, schedulerTiers, schedulerConfigurations)
	// Read-only close: the engine plans against the session but must NOT write back
	// PodGroup/Queue status (gang Unschedulable conditions, JobUpdater, queue
	// allocated) — that is the scheduler's job and a second writer would race it.
	defer schedframework.CloseSessionReadOnly(schedulerSession)

	klog.V(4).InfoS("repack: processing run", "run", run.Name, "mode", run.Spec.Mode)
	generation := run.Generation
	targetResource := e.resolveResource(run)
	if targetResource == "" {
		// No target accelerator resolvable: spec.goals is empty AND the engine's
		// --repack-default-resource is unset. Measuring fragmentation on the empty
		// resource would count every node as empty and silently report
		// NoFragmentation, so fail fast with an actionable reason instead.
		return e.fail(ctx, run, generation, "NoTargetResource",
			fmt.Errorf("no target accelerator resource: set spec.goals[0].resource or the engine flag --repack-default-resource"))
	}
	if !supportedTarget(targetResource) {
		// Only fully-qualified extended resources (nvidia.com/gpu, huawei.com/Ascend910)
		// are supported — they live in Resource.ScalarResources. Core compute resources
		// (cpu, memory, ephemeral-storage, ...) are stored in dedicated fields, so
		// Scalar() reads 0 for them and the run would be a silent no-op reporting
		// NoFragmentation. CEL rejects this on spec.goals, but the --repack-default-
		// resource fallback bypasses CEL, so guard it here too.
		return e.fail(ctx, run, generation, "UnsupportedResource",
			fmt.Errorf("target resource %q is not supported; only fully-qualified extended resources (e.g. nvidia.com/gpu) can be defragmented, not core resources like cpu/memory", targetResource))
	}
	if _, ok := engineframework.GetCore(e.config.Core); !ok {
		return e.fail(ctx, run, generation, "InvalidEngineConfiguration",
			fmt.Errorf("unknown repack core %q (registered: %v)", e.config.Core, engineframework.CoreNames()))
	}
	actions := e.config.Actions
	if len(actions) == 0 {
		actions = engineframework.DefaultActions()
	}
	for _, name := range actions {
		if _, ok := engineframework.GetAction(name); !ok {
			return e.fail(ctx, run, generation, "InvalidEngineConfiguration",
				fmt.Errorf("unknown repack action %q (registered: %v)", name, engineframework.ActionNames()))
		}
	}
	for _, name := range e.config.Plugins {
		if _, ok := engineframework.GetPlugin(name); !ok {
			return e.fail(ctx, run, generation, "InvalidEngineConfiguration",
				fmt.Errorf("unknown repack plugin %q", name))
		}
	}

	reason := state.ReasonSimulating
	if run.Spec.Mode == repackv1alpha1.RepackModeExecute {
		reason = state.ReasonEvicting
	}
	state.SetCondition(&run.Status.Conditions, state.CondQueued, metav1.ConditionFalse, state.ReasonSlotAcquired, "slot acquired", generation)
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionTrue, reason, "engine started", generation)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	if err := e.updateStatus(ctx, run); err != nil {
		return fmt.Errorf("persist Running status: %w", err)
	}

	scope, err := engineframework.NewScopeMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		return e.fail(ctx, run, generation, "ScopeError", err)
	}

	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	maxPodGroups, maxResource, hasPodGroupLimit, hasResourceLimit := maxPerRun(run, targetResource)
	klog.V(5).InfoS("repack: engine session opened", "run", run.Name, "resource", targetResource,
		"nodes", len(snapshot.Nodes()), "maxPodGroups", maxPodGroups, "maxResource", maxResource)
	engineSession := engineframework.OpenSession(engineframework.SessionConfig{
		Snapshot:                  snapshot,
		Run:                       run,
		Resource:                  targetResource,
		Mode:                      run.Spec.Mode,
		CoreName:                  e.config.Core,
		MinNodesFreed:             e.config.MinNodesFreed,
		MinFragImprovementPercent: minFragImprovement(run),
		MaxPodGroups:              maxPodGroups,
		MaxResource:               maxResource,
		LimitPodGroups:            hasPodGroupLimit,
		LimitResource:             hasResourceLimit,
		Hooks:                     hooksFor(run.Spec.Mode, e.schedulerCache.Client()),
		Free:                      adapter.NodeFreeCapacity,
	}, e.config.Plugins)
	engineSession.AddMovableFn(func(t *schedapi.TaskInfo) bool { return scope.InScope(t.Job) })
	defer engineframework.CloseSession(engineSession)

	// Actions are planning-only. Execute is committed below, after applyPlan has
	// been persisted successfully.
	engineframework.RunActions(e.config.Actions, engineSession)

	report, plan := engineSession.Report(), engineSession.Plan()
	klog.V(3).InfoS("plan computed", "run", run.Name, "mode", run.Spec.Mode, "resource", targetResource,
		"freedNodes", report.NodesFreed, "movedResource", report.MovedResource,
		"affectedPodGroups", report.AffectedPodGroups,
		"fragBeforePct", percentagePoints(report.FragmentationRateBefore), "fragAfterPct", percentagePoints(report.FragmentationRateAfter))
	// The Complete reason doubles as the "worth repacking?" verdict (§5.2.2);
	// there is no summary.verdict. worthwhile = the plan freed nodes; an empty
	// plan splits by whether fragmentation existed: none (clean) vs below the
	// benefit gate (fragmented but not worth acting on).
	worthwhile := report.NodesFreed > 0
	execute := run.Spec.Mode == repackv1alpha1.RepackModeExecute
	nominationTTL := time.Duration(0)
	if execute {
		nominationTTL = e.config.NominationTTL
	}
	applyPlan(run, report, plan, targetResource, execute, nominationTTL)
	if execute && worthwhile {
		// This is the prepare barrier. In particular, nominations must be visible
		// before an eviction can cause a replacement pod to appear.
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist prepared Execute plan: %w", err)
		}
	}

	var commitResult *engineframework.CommitResult
	if execute && worthwhile {
		result, err := engineframework.CommitPlan(plan, engineSession.Hooks())
		if err != nil {
			return e.fail(ctx, run, generation, state.ReasonExecuteFailed, err)
		}
		commitResult = &result
		engineSession.SetCommit(commitResult)
	}
	evictedCount, rejectedCount := 0, 0
	if commitResult != nil {
		evictedCount, rejectedCount = len(commitResult.Evicted), len(commitResult.Failed)
		metrics.ObserveEvictions(evictedCount, rejectedCount)
		klog.V(3).InfoS("evictions issued", "run", run.Name, "evictedCount", evictedCount, "rejectedCount", rejectedCount)
	}

	// Execute with a worthwhile plan: if every eviction was rejected (e.g. by PDBs)
	// the repack achieved nothing — fail rather than falsely reporting Executed.
	if execute && worthwhile {
		// Replace the optimistic prepared plan with the realized subset. Failed
		// evictions must not leave stale nominations or claim nodes were freed.
		plan = realizedPlan(plan, commitResult)
		report = engineframework.RenderReport(plan)
		applyPlan(run, report, plan, targetResource, true, nominationTTL)
	}
	if execute && worthwhile && evictedCount == 0 && rejectedCount > 0 {
		return e.fail(ctx, run, generation, state.ReasonExecuteFailed,
			fmt.Errorf("all %d evictions were rejected; no pods were moved", rejectedCount))
	}
	if execute && worthwhile && report.NodesFreed == 0 {
		return e.fail(ctx, run, generation, state.ReasonExecuteFailed,
			fmt.Errorf("evicted %d pods but no planned node was fully freed (%d evictions rejected)", evictedCount, rejectedCount))
	}

	var done, msg string
	switch {
	case !worthwhile && report.FragmentationRateBefore > 0:
		done, msg = state.ReasonBelowGoalThreshold, "engine finished"
	case !worthwhile:
		done, msg = state.ReasonNoFragmentation, "engine finished"
	case execute:
		done, msg = state.ReasonExecuted, "engine finished"
		// Partial success: some evictions were rejected but at least one succeeded.
		if rejectedCount > 0 {
			msg = fmt.Sprintf("evicted %d pods; %d evictions were rejected", evictedCount, rejectedCount)
		}
	default:
		done, msg = state.ReasonRepackRecommended, "engine finished"
	}
	state.SetCondition(&run.Status.Conditions, state.CondProgressing, metav1.ConditionFalse, done, msg, generation)
	state.SetCondition(&run.Status.Conditions, state.CondComplete, metav1.ConditionTrue, done, msg, generation)
	run.Status.Phase = state.DerivePhase(run.Status.Conditions)
	klog.V(3).InfoS("RepackRun finished", "run", run.Name, "mode", run.Spec.Mode, "outcome", done)
	return e.updateStatusTerminal(ctx, run)
}

// hooksFor returns the commit side effects. DryRun: none. Execute: evict each
// victim via the Eviction API (PDB-respecting; the workload controller then
// recreates the pod, steered by the nomination reconciler). No reservation/taint.
func hooksFor(mode repackv1alpha1.RepackMode, kubernetesClient kubernetes.Interface) engineframework.CommitHooks {
	if mode != repackv1alpha1.RepackModeExecute {
		return engineframework.CommitHooks{}
	}
	return engineframework.CommitHooks{
		Evict: func(move *engineapi.Move) error {
			if move == nil || move.Task == nil || move.Task.Pod == nil {
				return nil
			}
			pod := move.Task.Pod
			return kubernetesClient.PolicyV1().Evictions(pod.Namespace).Evict(context.Background(), &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			})
		},
	}
}

func (e *Engine) resolveResource(run *repackv1alpha1.RepackRun) v1.ResourceName {
	if len(run.Spec.Goals) > 0 && run.Spec.Goals[0].Resource != "" {
		return run.Spec.Goals[0].Resource
	}
	return v1.ResourceName(e.config.DefaultResource)
}

// supportedTarget reports whether targetResource is a defragmentable accelerator resource.
// Extended resources are fully qualified with a domain prefix (contain "/"), e.g.
// nvidia.com/gpu; core resources (cpu, memory, ephemeral-storage, pods,
// hugepages-*) are not and are unsupported. This mirrors the CEL rule on
// spec.goals[0].resource and also guards the --repack-default-resource fallback.
func supportedTarget(targetResource v1.ResourceName) bool {
	return strings.Contains(string(targetResource), "/")
}

// realizedPlan filters an optimistic plan through the actual eviction results.
// A node is reported freed only when every planned move sourced from that node
// was accepted. The returned plan retains the original fragmentation baseline so
// RenderReport can describe the realized benefit without inventing a new metric.
func realizedPlan(plan *engineapi.RepackPlan, commitResult *engineframework.CommitResult) *engineapi.RepackPlan {
	if plan == nil || commitResult == nil {
		return nil
	}
	succeeded := make(map[string]int, len(commitResult.Evicted))
	failedSource := make(map[string]bool, len(commitResult.Failed))
	for _, moveOutcome := range commitResult.Evicted {
		succeeded[moveOutcomeKey(moveOutcome.Namespace, moveOutcome.Task, moveOutcome.From, moveOutcome.To)]++
	}
	for _, moveOutcome := range commitResult.Failed {
		failedSource[moveOutcome.From] = true
	}

	realized := &engineapi.RepackPlan{Before: plan.Before}
	for _, m := range plan.Moves {
		if m == nil || m.Task == nil {
			continue
		}
		key := moveOutcomeKey(m.Task.Namespace, m.Task.Name, m.From, m.To)
		if succeeded[key] == 0 {
			continue
		}
		succeeded[key]--
		realized.Moves = append(realized.Moves, m)
	}
	for _, node := range plan.FreedNodes {
		if !failedSource[node] {
			realized.FreedNodes = append(realized.FreedNodes, node)
		}
	}
	for _, unit := range plan.FreedUnits {
		fullyFreed := true
		for _, node := range unit.Nodes {
			if failedSource[node] {
				fullyFreed = false
				break
			}
		}
		if fullyFreed {
			realized.FreedUnits = append(realized.FreedUnits, unit)
		}
	}
	realized.Cost = engineapi.CalculateDisruptionCost(realized.Moves, plan.Before.Resource)
	return realized
}

func moveOutcomeKey(namespace, task, from, to string) string {
	return namespace + "\x00" + task + "\x00" + from + "\x00" + to
}
