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

package engine

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// actionRuntime adapts controller infrastructure and durable executors to the
// framework.Runtime port. Workflow ordering deliberately remains in the Action.
type actionRuntime struct{ engine *Engine }

var _ engineframework.Runtime = (*actionRuntime)(nil)

func (e *Engine) actionRuntime() engineframework.Runtime { return &actionRuntime{engine: e} }

func (r *actionRuntime) OpenPlanningCycle(ctx context.Context, run *repackv1alpha1.RepackRun) (*engineframework.PlanningCycle, error) {
	e := r.engine
	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	if targetResource == "" {
		return nil, engineframework.NewActionError(state.ReasonInvalidConfiguration,
			fmt.Errorf("no target accelerator resource: set spec.goals[0].resource or the engine flag --repack-default-resource"))
	}
	if !engineconf.SupportedTarget(targetResource) {
		return nil, engineframework.NewActionError(state.ReasonInvalidConfiguration,
			fmt.Errorf("target resource %q is not supported; only fully-qualified extended resources can be defragmented", targetResource))
	}
	actions := e.config.Actions
	if len(actions) == 0 {
		actions = engineframework.DefaultActions()
	}
	if err := engineconf.ValidatePipeline(actions, e.config.Plugins); err != nil {
		return nil, engineframework.NewActionError(state.ReasonInvalidConfiguration, err)
	}

	schedulerSession := e.clusterCache.OpenSession(e.tiers, e.configurations)
	closed := false
	closeCycle := func() {
		if closed {
			return
		}
		closed = true
		schedframework.CloseSessionReadOnly(schedulerSession)
	}
	scope, err := enginescope.NewMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		closeCycle()
		return nil, engineframework.NewActionError(state.ReasonScopeResolutionFailed, err)
	}
	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	resolvedScope := enginestatus.BuildResolvedScope(snapshot.Nodes(), scope, targetResource)
	maxPodGroups, maxResource, hasPodGroupLimit, hasResourceLimit := engineconf.MaxPerRun(run, targetResource)
	ssn := engineframework.OpenSession(engineframework.SessionConfig{
		Context:                   ctx,
		Snapshot:                  snapshot,
		Run:                       run,
		Scope:                     scope,
		Resource:                  targetResource,
		Mode:                      run.Spec.Mode,
		MinNodesFreed:             e.config.MinNodesFreed,
		MinFragImprovementPercent: engineconf.MinFragImprovement(run),
		MaxPodGroups:              maxPodGroups,
		MaxResource:               maxResource,
		LimitPodGroups:            hasPodGroupLimit,
		LimitResource:             hasResourceLimit,
	}, e.config.Plugins)
	closePlanning := func() {
		engineframework.CloseSession(ssn)
		closeCycle()
	}
	if err := engineconf.ValidateSession(actions, e.config.Plugins, ssn); err != nil {
		closePlanning()
		return nil, engineframework.NewActionError(state.ReasonInvalidConfiguration, err)
	}
	return &engineframework.PlanningCycle{
		Session:       ssn,
		Resource:      targetResource,
		ResolvedScope: resolvedScope,
		Close:         closePlanning,
	}, nil
}

func (r *actionRuntime) UpdateStatus(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	return r.engine.updateStatus(ctx, run)
}

func (r *actionRuntime) UpdateTerminalStatus(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	return r.engine.updateStatusTerminal(ctx, run)
}

func (r *actionRuntime) Fail(ctx context.Context, run *repackv1alpha1.RepackRun, reason string, err error) error {
	return r.engine.fail(ctx, run, run.Generation, reason, err)
}

func (r *actionRuntime) ResolveMoveOwners(ctx context.Context, plan *engineapi.RepackPlan) map[string]*repackv1alpha1.WorkloadRef {
	return r.engine.resolveMoveOwners(ctx, plan)
}

func (r *actionRuntime) PrepareExecution(ctx context.Context, run *repackv1alpha1.RepackRun, plan *engineapi.RepackPlan, snapshot engineframework.Snapshot) error {
	e := r.engine
	policyReader, _ := snapshot.(enginestatus.PodGroupPlacementPolicyReader)
	if err := enginestatus.PrepareExecuteRelocations(run, plan, e.config.NominationTTL, e.now(), policyReader); err != nil {
		enginestatus.MarkExecuteNotPerformed(run)
		return engineframework.NewActionError(state.ReasonExecutionPreparationFailed,
			fmt.Errorf("prepare per-Pod relocation records: %w", err))
	}
	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	executionMessage := fmt.Sprintf(
		"Executing repack for %s: evicting %d Pods from %d PodGroups and moving %d cards to free %d nodes.",
		enginestatus.DisplayResource(targetResource), len(run.Status.Relocations), len(run.Status.Plan.Moves),
		run.Status.Plan.Summary.MovedCardCount, run.Status.Plan.Summary.FreedNodeCount)
	state.MarkRunning(run, state.ReasonEvicting, executionMessage)
	if err := e.updateStatus(ctx, run); err != nil {
		enginestatus.MarkExecuteNotPerformed(run)
		return fmt.Errorf("persist prepared Execute plan: %w", err)
	}
	preparedGroups := placementPodGroups(run)
	if err := e.preparePlacementLeases(ctx, run); err != nil {
		// Best-effort rollback reduces the recovery work, but the durable Pending
		// journal remains intact until cleanup succeeds. A retry can therefore
		// idempotently finish either preparation or rollback after a restart.
		if rollbackErr := e.releasePlacementLeases(ctx, run, preparedGroups); rollbackErr != nil {
			klog.ErrorS(rollbackErr, "repack: rollback partially prepared placement leases",
				"run", run.Name)
		}
		return fmt.Errorf("prepare placement leases: %w", err)
	}
	if err := e.setPlacementActive(ctx, run, true); err != nil {
		if rollbackErr := e.releasePlacementLeases(ctx, run, preparedGroups); rollbackErr != nil {
			klog.ErrorS(rollbackErr, "repack: rollback placement leases after discovery publication failure",
				"run", run.Name)
		}
		return fmt.Errorf("publish placement discovery: %w", err)
	}
	e.recordRunEvent(run, v1.EventTypeNormal, eventReasonExecutePrepared,
		fmt.Sprintf("Prepared %d replacement placement intents across %d PodGroups before eviction.",
			len(run.Status.Relocations), len(preparedGroups)))
	klog.V(3).InfoS("repack: Execute plan prepared before eviction",
		"run", run.Name, "moves", len(plan.Moves), "freedNodeCount", len(plan.FreedNodes),
		"relocationCount", len(run.Status.Relocations), "nominationTTL", e.config.NominationTTL)
	return nil
}

func (r *actionRuntime) ExecutePreparedEvictions(ctx context.Context, run *repackv1alpha1.RepackRun, resource v1.ResourceName) engineframework.RuntimeResult {
	return r.engine.executePreparedEvictions(ctx, run, run.Generation, resource)
}

func (r *actionRuntime) ResumePreparedEvictions(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	e := r.engine
	if err := e.preparePlacementLeases(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("resume placement leases before eviction: %w", err))
	}
	if err := e.setPlacementActive(ctx, run, true); err != nil {
		return runtimeError(fmt.Errorf("resume placement discovery before eviction: %w", err))
	}
	return e.executePreparedEvictions(ctx, run, run.Generation, engineconf.ResolveResource(run, e.config.DefaultResource))
}

func (r *actionRuntime) ReconcilePlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	e := r.engine
	groupsToRelease := placementGroupsDifference(plannedPodGroups(run), placementPodGroups(run))
	if err := e.releasePlacementLeases(ctx, run, groupsToRelease); err != nil {
		return runtimeError(fmt.Errorf("release unused placement leases before placement recovery: %w", err))
	}
	return e.reconcilePlacement(ctx, run)
}

func (r *actionRuntime) CleanupPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	return r.engine.cleanupPlacement(ctx, run)
}

func (r *actionRuntime) RecordPlanComputed(run *repackv1alpha1.RepackRun) {
	r.engine.recordRunEvent(run, v1.EventTypeNormal, eventReasonPlanComputed, plannedBenefitEventMessage(run))
}
