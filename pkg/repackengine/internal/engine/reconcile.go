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
	"runtime/debug"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	"volcano.sh/volcano/pkg/repackengine/metrics"
)

// enqueue adds a planning or in-flight placement RepackRun to the workqueue.
func (e *Engine) enqueue(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok || (!isCandidate(run) && !isPlacementCleanupCandidate(run)) {
		return
	}
	e.workQueue.Add(run.Name)
}

// isCandidate reports whether a run is ready for initial planning or for the
// post-eviction placement protocol. A Running Execute with a persisted plan is
// revisited only while it has durable placement records; it never repeats the
// eviction commit.
func isCandidate(run *repackv1alpha1.RepackRun) bool {
	if isEvictionCandidate(run) {
		return true
	}
	if isPlacementCandidate(run) {
		return true
	}
	p := run.Status.Phase
	return p == "" || p == repackv1alpha1.RepackPending ||
		(p == repackv1alpha1.RepackRunning && run.Status.Plan == nil)
}

func isEvictionCandidate(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		run.Status.Phase != repackv1alpha1.RepackRunning || run.Status.Plan == nil {
		return false
	}
	evictionJournalPresent := false
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Eviction.Phase != "" {
			evictionJournalPresent = true
		}
		switch run.Status.Relocations[index].Eviction.Phase {
		case repackv1alpha1.PodEvictionPending,
			repackv1alpha1.PodEvictionInProgress:
			return true
		}
	}
	if !evictionJournalPresent {
		return false
	}
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == state.ReasonReconcilingPlacements {
			return false
		}
	}
	// All per-Pod outcomes may be final while the accepted subset and the
	// ReconcilingPlacements barrier are not yet durable. Resume finalization.
	return true
}

func isPlacementCandidate(run *repackv1alpha1.RepackRun) bool {
	return run != nil && run.Spec.Mode == repackv1alpha1.RepackModeExecute &&
		run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.Plan != nil &&
		len(run.Status.Relocations) > 0 && !isEvictionCandidate(run)
}

// isPlacementCleanupCandidate admits an already-terminal Execute Run only to
// retry idempotent removal of its gate-owner markers and PodGroup leases. It
// never re-enters planning or eviction.
func isPlacementCleanupCandidate(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return false
	}
	// A failure before the first eviction clears relocations but can still leave
	// the admission discovery label or an original PodGroup lease behind. The
	// metadata label therefore also makes a terminal Run cleanup-retryable.
	if len(run.Status.Relocations) == 0 &&
		run.Labels[repackv1alpha1.PlacementActiveLabel] != "true" {
		return false
	}
	switch run.Status.Phase {
	case repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed:
		return true
	default:
		return false
	}
}

// maxReconcileRetries caps how many times a failing RepackRun is retried before
// it is treated as a poison pill: the engine gives up and marks it Failed rather
// than retrying forever (which would also keep re-panicking on a bad object).
const maxReconcileRetries = 5

// statusPersistenceRequeueInterval is the outer retry delay after bounded local
// status retries are exhausted. Status contention and terminal persistence
// failures must yield the worker without consuming the poison-pill budget.
const statusPersistenceRequeueInterval = time.Second

func (e *Engine) processNext(ctx context.Context) bool {
	key, shutdown := e.workQueue.Get()
	if shutdown {
		return false
	}
	defer e.workQueue.Done(key)

	if err := e.reconcileSafely(ctx, key); err != nil {
		if !reconcileErrorConsumesRetryBudget(err) {
			// AddAfter does not advance the rate-limiter counter, so status contention
			// cannot turn into ReconcileGaveUp. RetryOnConflict already performed
			// exponential backoff inside the status mutation; this delayed retry spans
			// reconcile attempts while preserving any prior real-failure count.
			klog.V(4).InfoS("requeueing RepackRun after retryable status persistence error",
				"run", key, "retryAfter", statusPersistenceRequeueInterval, "error", err)
			e.workQueue.AddAfter(key, statusPersistenceRequeueInterval)
			return true
		}
		utilruntime.HandleError(fmt.Errorf("repack-engine reconcile %q: %w", key, err))
		if e.workQueue.NumRequeues(key) < maxReconcileRetries {
			klog.V(4).InfoS("requeueing RepackRun after error", "run", key, "retries", e.workQueue.NumRequeues(key)+1)
			e.workQueue.AddRateLimited(key)
			return true
		}
		// Poison pill: stop retrying and fail the run so it does not loop forever
		// (and its Execute slot, if any, was already released by process's defer).
		e.workQueue.Forget(key)
		e.failByName(ctx, key, state.ReasonReconcileFailed, fmt.Errorf("gave up after %d retries: %w", maxReconcileRetries, err))
		return true
	}
	e.workQueue.Forget(key)
	return true
}

func reconcileErrorConsumesRetryBudget(err error) bool {
	return err != nil && !apierrors.IsConflict(err) && !isTerminalStatusPersistenceError(err)
}

// reconcileSafely runs reconcile with panic recovery so a single bad RepackRun
// (e.g. a plugin/snapshot panic) cannot crash the engine's worker goroutine. The
// panic is converted to an error; process's own defers (slot release, session
// close) still run during unwinding before it reaches here.
func (e *Engine) reconcileSafely(ctx context.Context, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in reconcile: %v", r)
			klog.ErrorS(err, "repack: recovered panic", "run", name, "stack", string(debug.Stack()))
		}
	}()
	return e.reconcile(ctx, name)
}

// failByName marks a run Failed by name (poison-pill path); best-effort.
func (e *Engine) failByName(ctx context.Context, name, reason string, cause error) {
	run, err := e.repackRunLister.Get(name)
	if err != nil {
		return // gone or lister error; nothing to write
	}
	work := run.DeepCopy()
	if err := e.fail(ctx, work, work.Generation, reason, cause); err != nil {
		klog.ErrorS(err, "repack: persist poison-pill failure", "run", name)
		if isTerminalStatusPersistenceError(err) {
			e.workQueue.AddAfter(name, statusPersistenceRequeueInterval)
		}
	}
}

// reconcile processes one RepackRun: re-check it's still a candidate, apply the
// Execute serialization gate (one-at-a-time + cooldown — it lives here, in the
// worker that actually evicts), then plan/act.
func (e *Engine) reconcile(ctx context.Context, name string) error {
	run, err := e.repackRunLister.Get(name)
	if apierrors.IsNotFound(err) {
		e.forgetPendingTerminalStatus(name)
		return nil
	}
	if err != nil {
		return err
	}
	if desired, found := e.pendingTerminalStatus(name); found {
		work := run.DeepCopy()
		desired.DeepCopyInto(&work.Status)
		if err := e.updateStatusTerminal(ctx, work); err != nil {
			return err
		}
		if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
			e.markExecuteDone(work.Name)
			e.requeueGatedRuns()
			return e.cleanupPlacement(ctx, work)
		}
		return nil
	}
	if isPlacementCleanupCandidate(run) {
		return e.cleanupPlacement(ctx, run.DeepCopy())
	}
	if !isCandidate(run) {
		return nil // already picked up / terminal
	}
	klog.V(4).InfoS("reconciling RepackRun", "run", name, "mode", run.Spec.Mode)
	work := run.DeepCopy()

	// Acknowledge as Pending so `kubectl get repackrun` shows a phase before the
	// engine starts (deferred Execute runs also settle here via the gate below).
	if work.Status.Phase == "" {
		work.Status.Phase = repackv1alpha1.RepackPending
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
	}

	active, lastFinish := false, time.Time{}
	gate := state.GateDecision{Admit: true} // DryRun is never serialized by Execute.
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		gate, active, lastFinish = e.tryAcquireExecute(work.Name, e.now())
	}
	klog.V(4).InfoS("repack: execute gate evaluated", "run", work.Name, "mode", work.Spec.Mode,
		"executeActive", active, "lastExecuteFinish", lastFinish, "cooldown", e.config.Cooldown,
		"admit", gate.Admit, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
	if !gate.Admit {
		metrics.ObserveGateRejection(gate.Reason)
		klog.V(3).InfoS("RepackRun deferred by execute gate",
			"run", name, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
		message := "Waiting to execute: another Execute RepackRun is active; this run will be retried when the active run finishes."
		if gate.Reason == state.ReasonExecuteCooldownActive {
			message = fmt.Sprintf(
				"Waiting to execute: the previous Execute RepackRun is cooling down; retrying after %s.",
				gate.RequeueAfter.Round(time.Second))
		}
		conditionChanged := state.MarkPending(work, gate.Reason, message)
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
		if conditionChanged {
			e.recordRunEvent(work, v1.EventTypeNormal, gate.Reason, message)
		}
		if gate.RequeueAfter > 0 {
			e.workQueue.AddAfter(name, gate.RequeueAfter)
		}
		return nil
	}
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
		klog.V(3).InfoS("repack: Execute slot acquired", "run", work.Name, "cooldown", e.config.Cooldown)
	}
	if isEvictionCandidate(work) {
		// The prepared status may have been persisted immediately before a crash,
		// while lease publication was still in progress. Re-establish both halves
		// of the admission barrier idempotently before resuming any API call.
		if err := e.preparePlacementLeases(ctx, work); err != nil {
			return fmt.Errorf("resume placement leases before eviction: %w", err)
		}
		if err := e.setPlacementActive(ctx, work, true); err != nil {
			return fmt.Errorf("resume placement discovery before eviction: %w", err)
		}
		return e.executePreparedEvictions(ctx, work, work.Generation, engineconf.ResolveResource(work, e.config.DefaultResource))
	}
	if isPlacementCandidate(work) {
		// A crash can occur after the accepted relocation subset becomes durable
		// but before leases for rejected PodGroups are released. Reconcile that
		// one-way cleanup before placement; retained groups remain protected.
		groupsToRelease := placementGroupsDifference(plannedPodGroups(work), placementPodGroups(work))
		if err := e.releasePlacementLeases(ctx, work, groupsToRelease); err != nil {
			return fmt.Errorf("release unused placement leases before placement recovery: %w", err)
		}
		return e.reconcilePlacement(ctx, work)
	}
	return e.planRun(ctx, work)
}
